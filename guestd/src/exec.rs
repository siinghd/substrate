use crate::messages::*;
use crate::protocol::*;
use std::io::{Read, Write};
use std::os::unix::io::{IntoRawFd};
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;

pub type SignalMsg = ExecSignal;

fn exit_msg(exec_id: &str, code: i32, killed: bool) -> ExecExit {
    ExecExit { exec_id: exec_id.into(), code, killed }
}

fn eof_meta(fd: &'static str) -> ExecDataMeta {
    ExecDataMeta { exec_id: String::new(), fd: fd.into(), eof: true }
}

pub struct ExecManager {
    procs: Mutex<std::collections::HashMap<String, i32>>,
}

fn sig_num(name: &str) -> i32 {
    match name.to_uppercase().as_str() {
        "HUP" => 1,
        "INT" => 2,
        "QUIT" => 3,
        "KILL" => 9,
        "USR1" => 10,
        "USR2" => 12,
        "PIPE" => 13,
        "TERM" => 15,
        "CONT" => 18,
        "STOP" => 19,
        other => other.parse().unwrap_or(15),
    }
}

impl ExecManager {
    pub fn new() -> Self {
        ExecManager { procs: Mutex::new(std::collections::HashMap::new()) }
    }

    pub fn kill(&self, exec_id: &str, signal: &str) {
        let pgid = self.procs.lock().unwrap().get(exec_id).copied();
        if let Some(pgid) = pgid {
            unsafe {
                libc::kill(-pgid, sig_num(signal));
            }
        }
    }
}

struct FdReader(i32);
impl Read for FdReader {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let n = unsafe { libc::read(self.0, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
        if n < 0 {
            Err(std::io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }
}
unsafe impl Send for FdReader {}

struct FdWriter(i32);
impl Write for FdWriter {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        let n = unsafe { libc::write(self.0, buf.as_ptr() as *const libc::c_void, buf.len()) };
        if n < 0 {
            Err(std::io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}
unsafe impl Send for FdWriter {}

fn pump_to_stream(mut r: Box<dyn Read + Send>, stream: Stream, tag: u8, outstanding: Arc<AtomicUsize>, eof_fd: &'static str) {
    outstanding.fetch_add(1, Ordering::SeqCst);
    thread::spawn(move || {
        let mut buf = [0u8; 16384];
        loop {
            match r.read(&mut buf) {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    let mut frame = Vec::with_capacity(n + 1);
                    frame.push(tag);
                    frame.extend_from_slice(&buf[..n]);
                    if stream.send_binary(OP_EXEC_DATA, frame).is_err() {
                        break;
                    }
                }
            }
        }
        let _ = stream.send(OP_EXEC_DATA, serde_json::to_vec(&eof_meta(eof_fd)).unwrap_or_default());
        outstanding.fetch_sub(1, Ordering::SeqCst);
    });
}

enum Sink {
    Pipe(Mutex<FdWriter>),
    Pty(Mutex<FdWriter>),
}

impl Sink {
    fn write_all(&self, b: &[u8]) -> std::io::Result<()> {
        match self {
            Sink::Pipe(m) => m.lock().unwrap().write_all(b),
            Sink::Pty(m) => m.lock().unwrap().write_all(b),
        }
    }
    /// Close the child's stdin so readers blocking on EOF wake up.
    fn close_stdin(&self) {
        match self {
            // we own the fd via into_raw_fd(); closing it sends EOF to the child
            Sink::Pipe(m) => {
                let w = m.lock().unwrap();
                unsafe {
                    libc::close(w.0);
                }
            }
            // closing the pty master would kill the session; leave open
            Sink::Pty(_) => {}
        }
    }
}

pub fn handle(mgr: &ExecManager, stream: Stream, first: Frame) {
    let req: ExecRequest = match serde_json::from_slice::<ExecRequest>(&first.payload) {
        Ok(r) if !r.argv.is_empty() => r,
        _ => {
            let _ = stream.send_fin(OP_EXEC_EXIT, serde_json::to_vec(&exit_msg("", 125, false)).unwrap());
            return;
        }
    };
    let exec_id = req.exec_id.clone();
    let tty = req.tty;
    let argv = req.argv.clone();

    let mut cmd = Command::new(&argv[0]);
    cmd.args(&argv[1..]);
    if !req.cwd.is_empty() {
        cmd.current_dir(&req.cwd);
    }
    cmd.env_clear();
    cmd.env("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin");
    cmd.env("HOME", "/root");
    cmd.env("TERM", if tty { "xterm-256color" } else { "dumb" });
    for (k, v) in &req.env {
        cmd.env(k, v);
    }
    cmd.stdin(Stdio::piped());

    let mut master_fd: i32 = -1;
    if tty {
        let mut m: i32 = -1;
        let mut s: i32 = -1;
        unsafe {
            libc::openpty(&mut m, &mut s, std::ptr::null_mut(), std::ptr::null(), std::ptr::null());
        }
        master_fd = m;
        let (rows, cols) = (req.rows, req.cols);
        unsafe {
            cmd.pre_exec(move || {
                libc::setsid();
                libc::dup2(s, 0);
                libc::dup2(s, 1);
                libc::dup2(s, 2);
                if s > 2 {
                    libc::close(s);
                }
                libc::ioctl(0, libc::TIOCSCTTY, 0);
                if rows > 0 && cols > 0 {
                    let ws = libc::winsize { ws_row: rows, ws_col: cols, ws_xpixel: 0, ws_ypixel: 0 };
                    libc::ioctl(0, libc::TIOCSWINSZ, &ws);
                }
                Ok(())
            });
        }
    } else {
        cmd.stdout(Stdio::piped());
        cmd.stderr(Stdio::piped());
        unsafe {
            cmd.pre_exec(|| {
                libc::setsid();
                Ok(())
            });
        }
    }

    let mut child: Child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            let mut frame = vec![2u8];
            frame.extend_from_slice(format!("guestd: spawn failed: {}\n", e).as_bytes());
            let _ = stream.send_binary(OP_EXEC_DATA, frame);
            let _ = stream.send_fin(OP_EXEC_EXIT, serde_json::to_vec(&exit_msg(&exec_id, 126, false)).unwrap());
            return;
        }
    };

    let pid = child.id() as i32;
    mgr.procs.lock().unwrap().insert(exec_id.clone(), pid);

    let outstanding = Arc::new(AtomicUsize::new(0));

    let sink: Sink = if tty {
        drop(child.stdin.take());
        Sink::Pty(Mutex::new(FdWriter(master_fd)))
    } else {
        let raw = child.stdin.take().unwrap().into_raw_fd();
        Sink::Pipe(Mutex::new(FdWriter(raw)))
    };

    if tty {
        pump_to_stream(Box::new(FdReader(master_fd)), stream.clone(), 1, outstanding.clone(), "out");
    } else {
        let stdout_fd = child.stdout.take().unwrap().into_raw_fd();
        let stderr_fd = child.stderr.take().unwrap().into_raw_fd();
        pump_to_stream(Box::new(FdReader(stdout_fd)), stream.clone(), 1, outstanding.clone(), "out");
        pump_to_stream(Box::new(FdReader(stderr_fd)), stream.clone(), 2, outstanding.clone(), "err");
    }

    let (exit_tx, exit_rx) = std::sync::mpsc::channel::<(i32, bool)>();
    thread::spawn(move || {
        let mut status: i32 = 0;
        let r = unsafe { libc::waitpid(pid, &mut status, 0) };
        let code = if r == pid {
            if libc::WIFEXITED(status) {
                libc::WEXITSTATUS(status)
            } else if libc::WIFSIGNALED(status) {
                128 + libc::WTERMSIG(status)
            } else {
                -1
            }
        } else {
            -1
        };
        let killed = r == pid && libc::WIFSIGNALED(status);
        let _ = exit_tx.send((code, killed));
    });

    let mut final_code: Option<(i32, bool)> = None;
    let mut host_gone = false;
    while final_code.is_none() {
        match stream.recv_timeout(Duration::from_millis(50)) {
            Some(f) if f.op == OP_EXEC_DATA && f.has(FLAG_BINARY) => {
                if f.payload.len() > 1 {
                    let _ = sink.write_all(&f.payload[1..]);
                }
                if f.has(FLAG_FIN) {
                    sink.close_stdin();
                }
            }
            Some(f) if f.op == OP_EXEC_SIGNAL => {
                if let Ok(sig) = serde_json::from_slice::<ExecSignal>(&f.payload) {
                    mgr.kill(&exec_id, &sig.signal);
                }
            }
            Some(f) if f.op == OP_EXEC_RESIZE && tty => {
                if let Ok(rs) = serde_json::from_slice::<ExecResize>(&f.payload) {
                    let ws = libc::winsize { ws_row: rs.rows, ws_col: rs.cols, ws_xpixel: 0, ws_ypixel: 0 };
                    unsafe { libc::ioctl(master_fd, libc::TIOCSWINSZ, &ws); }
                }
            }
            Some(_) => {}
            None => {
                if stream.is_closed() {
                    host_gone = true;
                    break;
                }
            }
        }
        if let Ok(pair) = exit_rx.try_recv() {
            final_code = Some(pair);
        }
    }

    if final_code.is_none() {
        mgr.kill(&exec_id, "KILL");
        final_code = exit_rx.recv_timeout(Duration::from_secs(2)).ok();
    }

    mgr.procs.lock().unwrap().remove(&exec_id);

    if host_gone {
        return;
    }

    let deadline = std::time::Instant::now() + Duration::from_secs(2);
    while outstanding.load(Ordering::SeqCst) > 0 && std::time::Instant::now() < deadline {
        thread::sleep(Duration::from_millis(5));
    }

    let (code, killed) = final_code.unwrap_or((-1, false));
    let _ = stream.send_fin(OP_EXEC_EXIT, serde_json::to_vec(&exit_msg(&exec_id, code, killed)).unwrap());
}
