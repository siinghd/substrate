use crate::messages::*;
use crate::protocol::*;
use std::fs;
use std::io::{Read, Seek, SeekFrom, Write};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};

const MAX_READ_CHUNK: usize = 1 << 20;
const DEFAULT_MAX_READ: i64 = 32 << 20;

pub fn handle(mut stream: Stream, first: Frame) {
    let req: FsRequest = match serde_json::from_slice(&first.payload) {
        Ok(r) => r,
        Err(e) => {
            let _ = respond(&mut stream, FsResponse { ok: false, error: format!("bad request: {}", e), size: 0, entries: vec![] });
            return;
        }
    };

    // Collect any binary write payload until the host closes its side.
    let mut write_buf: Vec<u8> = Vec::new();
    let expects_data = matches!(req.op.as_str(), "write");
    if expects_data {
        loop {
            match stream.recv() {
                Some(f) if f.op == OP_FS_DATA && f.has(FLAG_BINARY) => {
                    if !f.has(FLAG_FIN) {
                        write_buf.extend_from_slice(&f.payload);
                    } else {
                        break;
                    }
                }
                Some(f) if f.has(FLAG_FIN) => break,
                Some(_) => {}
                None => break,
            }
        }
    }

    let resp = dispatch(&req, &write_buf);
    match resp {
        Outcome::Json(json) => {
            let _ = stream.send_fin(OP_FS_RESP, json);
        }
        Outcome::Bytes(bytes, size) => {
            for chunk in bytes.chunks(MAX_READ_CHUNK) {
                if stream.send_binary(OP_FS_DATA, chunk.to_vec()).is_err() {
                    return;
                }
            }
            let fin = FsResponse { ok: true, error: String::new(), size, entries: vec![] };
            let _ = stream.send_fin(OP_FS_RESP, serde_json::to_vec(&fin).unwrap());
        }
    }
}

enum Outcome {
    Json(Vec<u8>),
    Bytes(Vec<u8>, i64),
}

fn ok_resp(size: i64) -> FsResponse {
    FsResponse { ok: true, error: String::new(), size, entries: vec![] }
}

fn err_resp(msg: String) -> FsResponse {
    FsResponse { ok: false, error: msg, size: 0, entries: vec![] }
}

fn respond(stream: &mut Stream, r: FsResponse) -> std::io::Result<()> {
    stream.send_fin(OP_FS_RESP, serde_json::to_vec(&r).unwrap())
}

fn dispatch(req: &FsRequest, data: &[u8]) -> Outcome {
    match req.op.as_str() {
        "read" => read(req),
        "list" => list(req),
        "write" => write(req, data),
        "remove" => remove(req),
        "mkdir" => mkdir(req),
        "stat" => stat(req),
        other => Outcome::Json(serde_json::to_vec(&err_resp(format!("unknown op {}", other))).unwrap()),
    }
}

fn read(req: &FsRequest) -> Outcome {
    let mut f = match fs::File::open(&req.path) {
        Ok(f) => f,
        Err(e) => return Outcome::Json(serde_json::to_vec(&err_resp(e.to_string())).unwrap()),
    };
    let meta = match f.metadata() {
        Ok(m) => m,
        Err(e) => return Outcome::Json(serde_json::to_vec(&err_resp(e.to_string())).unwrap()),
    };
    if meta.is_dir() {
        return Outcome::Json(serde_json::to_vec(&err_resp("is a directory".into())).unwrap());
    }
    let offset = req.offset.max(0);
    if offset > 0 && f.seek(SeekFrom::Start(offset as u64)).is_err() {
        return Outcome::Json(serde_json::to_vec(&err_resp("seek failed".into())).unwrap());
    }
    let want = if req.length > 0 { req.length.min(DEFAULT_MAX_READ) } else { DEFAULT_MAX_READ };
    let mut buf = Vec::with_capacity(want.min(8 << 20) as usize);
    let mut take = f.take(want as u64);
    match take.read_to_end(&mut buf) {
        Ok(_) => {
            let n = buf.len() as i64;
            Outcome::Bytes(buf, n)
        }
        Err(_) => Outcome::Json(serde_json::to_vec(&err_resp("read failed".into())).unwrap()),
    }
}

fn list(req: &FsRequest) -> Outcome {
    const MAX_ENTRIES: usize = 2000;
    let rd = match fs::read_dir(&req.path) {
        Ok(rd) => rd,
        Err(e) => return Outcome::Json(serde_json::to_vec(&err_resp(e.to_string())).unwrap()),
    };
    let mut entries: Vec<FsEntry> = Vec::new();
    for e in rd.flatten() {
        if entries.len() >= MAX_ENTRIES {
            break;
        }
        let name = e.file_name().to_string_lossy().into_owned();
        let (size, dir) = match e.metadata() {
            Ok(m) => (m.len() as i64, m.is_dir()),
            Err(_) => (0, false),
        };
        entries.push(FsEntry { name, size, dir });
    }
    entries.sort_by(|a, b| b.dir.cmp(&a.dir).then(a.name.cmp(&b.name)));
    Outcome::Json(serde_json::to_vec(&FsResponse { ok: true, error: String::new(), size: entries.len() as i64, entries }).unwrap())
}

fn write(req: &FsRequest, data: &[u8]) -> Outcome {
    let res = (|| -> std::io::Result<i64> {
        if let Some(parent) = std::path::Path::new(&req.path).parent() {
            fs::create_dir_all(parent)?;
        }
        let mut opts = fs::OpenOptions::new();
        opts.write(true).create(true);
        if req.append {
            opts.append(true);
        } else {
            opts.truncate(true);
        }
        if req.mode > 0 {
            opts.mode(req.mode);
        }
        let mut f = opts.open(&req.path)?;
        f.write_all(data)?;
        Ok(data.len() as i64)
    })();
    match res {
        Ok(size) => Outcome::Json(serde_json::to_vec(&ok_resp(size)).unwrap()),
        Err(e) => Outcome::Json(serde_json::to_vec(&err_resp(e.to_string())).unwrap()),
    }
}

fn remove(req: &FsRequest) -> Outcome {
    let p = std::path::Path::new(&req.path);
    let res = if p.is_dir() { fs::remove_dir_all(p) } else { fs::remove_file(p) };
    match res {
        Ok(_) => Outcome::Json(serde_json::to_vec(&ok_resp(0)).unwrap()),
        Err(e) => Outcome::Json(serde_json::to_vec(&err_resp(e.to_string())).unwrap()),
    }
}

fn mkdir(req: &FsRequest) -> Outcome {
    match fs::create_dir_all(&req.path) {
        Ok(_) => Outcome::Json(serde_json::to_vec(&ok_resp(0)).unwrap()),
        Err(e) => Outcome::Json(serde_json::to_vec(&err_resp(e.to_string())).unwrap()),
    }
}

fn stat(req: &FsRequest) -> Outcome {
    match fs::metadata(&req.path) {
        Ok(m) => {
            let mode = m.permissions().mode();
            let entry = FsEntry {
                name: std::path::Path::new(&req.path)
                    .file_name()
                    .map(|s| s.to_string_lossy().into_owned())
                    .unwrap_or_else(|| req.path.clone()),
                size: m.size() as i64,
                dir: m.is_dir(),
            };
            let _ = mode;
            Outcome::Json(serde_json::to_vec(&FsResponse { ok: true, error: String::new(), size: entry.size, entries: vec![entry] }).unwrap())
        }
        Err(e) => Outcome::Json(serde_json::to_vec(&err_resp(e.to_string())).unwrap()),
    }
}
