use std::collections::{HashMap, VecDeque};
use std::io::{Read, Write};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, Sender};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::time::Duration;

pub const MAX_FRAME_SIZE: u32 = 1 << 20;
pub const HEADER_LEN: usize = 13;

pub const FLAG_SYN: u8 = 1;
pub const FLAG_FIN: u8 = 2;
pub const FLAG_BINARY: u8 = 4;

pub const OP_PING: u8 = 0;
pub const OP_PONG: u8 = 1;
pub const OP_HELLO: u8 = 2;
pub const OP_EXEC_REQ: u8 = 3;
pub const OP_EXEC_DATA: u8 = 4;
pub const OP_EXEC_EXIT: u8 = 5;
pub const OP_EXEC_SIGNAL: u8 = 6;
pub const OP_FS_REQ: u8 = 7;
pub const OP_FS_DATA: u8 = 8;
pub const OP_FS_RESP: u8 = 9;
pub const OP_PORTS: u8 = 10;
pub const OP_METRICS: u8 = 11;
pub const OP_RESEED: u8 = 12;
pub const OP_RESEED_ACK: u8 = 13;
pub const OP_EXEC_RESIZE: u8 = 14;

#[derive(Clone)]
pub struct Frame {
    pub stream_id: u32,
    pub flags: u8,
    pub op: u8,
    pub payload: Vec<u8>,
}

impl Frame {
    pub fn has(&self, flag: u8) -> bool {
        self.flags & flag != 0
    }
}

pub fn write_frame(w: &mut dyn Write, f: &Frame) -> std::io::Result<()> {
    if f.payload.len() as u32 > MAX_FRAME_SIZE {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            "frame too large",
        ));
    }
    let mut hdr = [0u8; HEADER_LEN];
    hdr[0..4].copy_from_slice(&f.stream_id.to_le_bytes());
    hdr[4] = f.flags;
    hdr[5] = f.op;
    hdr[6..10].copy_from_slice(&(f.payload.len() as u32).to_le_bytes());
    w.write_all(&hdr)?;
    if !f.payload.is_empty() {
        w.write_all(&f.payload)?;
    }
    Ok(())
}

pub fn read_frame<R: Read>(r: &mut R) -> std::io::Result<Frame> {
    let mut hdr = [0u8; HEADER_LEN];
    r.read_exact(&mut hdr)?;
    let stream_id = u32::from_le_bytes(hdr[0..4].try_into().unwrap());
    let flags = hdr[4];
    let op = hdr[5];
    let len = u32::from_le_bytes(hdr[6..10].try_into().unwrap());
    if len > MAX_FRAME_SIZE {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            "frame too large",
        ));
    }
    let mut payload = vec![0u8; len as usize];
    if len > 0 {
        r.read_exact(&mut payload)?;
    }
    Ok(Frame { stream_id, flags, op, payload })
}

const QUEUE_CAP: usize = 256;

struct StreamInner {
    id: u32,
    queue: Mutex<VecDeque<Frame>>,
    cv: Condvar,
    closed: Mutex<bool>,
    /// Exactly one FIN per stream reaches the peer, whichever of send_fin or
    /// close gets there first. Shared, so cloned handles cannot double-send.
    fin_sent: AtomicBool,
}

/// How long the demux thread will wait for one slow consumer before giving up
/// on that stream. It must never wait forever: `read_loop` is the ONLY reader
/// of the vsock, so parking it stops every other stream on the sandbox too.
const PUSH_TIMEOUT: Duration = Duration::from_secs(30);

impl StreamInner {
    fn push(self: &Arc<Self>, f: Frame) -> bool {
        let mut q = self.queue.lock().unwrap();
        let deadline = std::time::Instant::now() + PUSH_TIMEOUT;
        loop {
            if *self.closed.lock().unwrap() {
                return false;
            }
            if q.len() < QUEUE_CAP {
                q.push_back(f);
                drop(q);
                self.cv.notify_all();
                return true;
            }
            let now = std::time::Instant::now();
            if now >= deadline {
                // Consumer is gone or hopelessly behind: fail THIS stream so
                // the demux thread lives on.
                return false;
            }
            let (nq, _) = self.cv.wait_timeout(q, deadline - now).unwrap();
            q = nq;
        }
    }

    fn mark_closed(&self) {
        *self.closed.lock().unwrap() = true;
        self.cv.notify_all();
    }
}

struct Shared {
    streams: Mutex<HashMap<u32, Arc<StreamInner>>>,
    incoming_tx: Mutex<Option<Sender<(u32, Arc<StreamInner>)>>>,
    dead: Mutex<bool>,
}

pub struct Mux {
    writer: Mutex<Box<dyn Write + Send>>,
    shared: Arc<Shared>,
    incoming_rx: Mutex<Receiver<(u32, Arc<StreamInner>)>>,
    next_id: Mutex<u32>,
}

#[derive(Clone)]
pub struct Stream {
    inner: Arc<StreamInner>,
    mux: Arc<Mux>,
}

impl Mux {
    pub fn new(r: Box<dyn Read + Send>, w: Box<dyn Write + Send>, initiator: bool) -> Arc<Mux> {
        let (tx, rx) = mpsc::channel();
        let mux = Arc::new(Mux {
            writer: Mutex::new(w),
            shared: Arc::new(Shared {
                streams: Mutex::new(HashMap::new()),
                incoming_tx: Mutex::new(Some(tx)),
                dead: Mutex::new(false),
            }),
            incoming_rx: Mutex::new(rx),
            next_id: Mutex::new(if initiator { 1 } else { 2 }),
        });
        let m2 = mux.clone();
        std::thread::spawn(move || read_loop(m2, r));
        mux
    }

    pub fn open(self: &Arc<Mux>, op: u8, payload: Vec<u8>) -> std::io::Result<Stream> {
        let id = {
            let mut n = self.next_id.lock().unwrap();
            let id = *n;
            *n = n.wrapping_add(2);
            id
        };
        let st = self.register(id);
        self.send_raw(&Frame { stream_id: id, flags: FLAG_SYN, op, payload })?;
        Ok(Stream { inner: st, mux: self.clone() })
    }

    pub fn accept(self: &Arc<Mux>) -> Option<Stream> {
        loop {
            if self.is_dead() {
                return None;
            }
            let guard = self.incoming_rx.lock();
            let rx = guard.unwrap_or_else(|e| e.into_inner());
            match rx.recv_timeout(Duration::from_millis(200)) {
                Ok((_id, st)) => {
                    return Some(Stream { inner: st, mux: self.clone() });
                }
                Err(RecvTimeoutError::Timeout) => {}
                Err(RecvTimeoutError::Disconnected) => return None,
            }
        }
    }

    fn register(self: &Arc<Mux>, id: u32) -> Arc<StreamInner> {
        let st = Arc::new(StreamInner {
            id,
            queue: Mutex::new(VecDeque::new()),
            cv: Condvar::new(),
            closed: Mutex::new(false),
            fin_sent: AtomicBool::new(false),
        });
        self.shared.streams.lock().unwrap().insert(id, st.clone());
        st
    }

    fn send_raw(&self, f: &Frame) -> std::io::Result<()> {
        let mut w = self.writer.lock().unwrap();
        write_frame(w.as_mut(), f)
    }

    /// Number of streams the mux is still tracking. Test-facing: an
    /// ever-growing table is the leak this exists to catch.
    pub fn live_stream_count(&self) -> usize {
        self.shared.streams.lock().unwrap().len()
    }

    fn drop_stream(&self, id: u32) {
        if let Some(st) = self.shared.streams.lock().unwrap().remove(&id) {
            st.mark_closed();
        }
    }

    pub fn is_dead(&self) -> bool {
        *self.shared.dead.lock().unwrap()
    }

    fn die(&self) {
        let mut d = self.shared.dead.lock().unwrap();
        if *d {
            return;
        }
        *d = true;
        drop(d);
        *self.shared.incoming_tx.lock().unwrap() = None;
        let map = self.shared.streams.lock().unwrap();
        for (_, st) in map.iter() {
            st.mark_closed();
        }
    }
}

fn read_loop(mux: Arc<Mux>, mut r: Box<dyn Read + Send>) {
    loop {
        let f = match read_frame(&mut r) {
            Ok(f) => f,
            Err(_) => break,
        };
        let syn = f.has(FLAG_SYN);
        let f_id = f.stream_id;
        let existing = mux.shared.streams.lock().unwrap().get(&f_id).cloned();
        let st = match existing {
            Some(st) => st,
            None if syn => {
                let st = mux.register(f.stream_id);
                let tx = mux.shared.incoming_tx.lock().unwrap().clone();
                if let Some(tx) = tx {
                    let _ = tx.send((f.stream_id, st.clone()));
                }
                st
            }
            None => continue,
        };
        let fin = f.has(FLAG_FIN);
        if !st.push(f) {
            // Stalled or already closed: drop the stream, keep the mux.
            mux.drop_stream(f_id);
            continue;
        }
        if fin {
            st.mark_closed();
            // Reclaim the map entry. The Go side does this on FIN; guestd did
            // not, so `shared.streams` grew for the life of the connection —
            // an unbounded map inside a 512MiB guest.
            mux.shared.streams.lock().unwrap().remove(&f_id);
        }
    }
    mux.die();
}

impl Stream {
    #[allow(dead_code)]
    pub fn id(&self) -> u32 {
        self.inner.id
    }

    pub fn recv(&self) -> Option<Frame> {
        let mut q = self.inner.queue.lock().unwrap();
        loop {
            if let Some(f) = q.pop_front() {
                drop(q);
                // MUST notify: a producer blocked because the queue was full
                // is waiting on exactly this condvar. Popping without
                // notifying was a lost wakeup — once a stream hit QUEUE_CAP,
                // `read_loop` parked in push() and never woke, even after the
                // queue drained. That is the single vsock reader, so the whole
                // sandbox's control channel wedged.
                self.inner.cv.notify_all();
                return Some(f);
            }
            if *self.inner.closed.lock().unwrap() {
                return None;
            }
            q = self.inner.cv.wait(q).unwrap();
        }
    }

    pub fn recv_timeout(&self, dur: Duration) -> Option<Frame> {
        let mut q = self.inner.queue.lock().unwrap();
        let deadline = std::time::Instant::now() + dur;
        loop {
            if let Some(f) = q.pop_front() {
                drop(q);
                self.inner.cv.notify_all(); // see recv(): freeing capacity must wake the producer
                return Some(f);
            }
            if *self.inner.closed.lock().unwrap() {
                return None;
            }
            let now = std::time::Instant::now();
            if now >= deadline {
                return None;
            }
            let (nq, _) = self.inner.cv.wait_timeout(q, deadline - now).unwrap();
            q = nq;
        }
    }

    pub fn send(&self, op: u8, payload: Vec<u8>) -> std::io::Result<()> {
        self.mux.send_raw(&Frame { stream_id: self.inner.id, flags: 0, op, payload })
    }

    pub fn send_binary(&self, op: u8, payload: Vec<u8>) -> std::io::Result<()> {
        self.mux.send_raw(&Frame { stream_id: self.inner.id, flags: FLAG_BINARY, op, payload })
    }

    pub fn send_fin(&self, op: u8, payload: Vec<u8>) -> std::io::Result<()> {
        self.send_fin_once(op, payload)
    }

    /// Close ends the stream for BOTH sides. It used to be a local map removal
    /// that put nothing on the wire, so the peer — which reclaims its stream
    /// table only on FIN — kept one ghost entry per closed stream, forever.
    pub fn close(&self) {
        let _ = self.send_fin_once(OP_PING, Vec::new());
        self.mux.drop_stream(self.inner.id);
    }

    fn send_fin_once(&self, op: u8, payload: Vec<u8>) -> std::io::Result<()> {
        if self.inner.fin_sent.swap(true, Ordering::SeqCst) {
            return Ok(());
        }
        self.mux.send_raw(&Frame { stream_id: self.inner.id, flags: FLAG_FIN, op, payload })
    }

    pub fn is_closed(&self) -> bool {
        let q = self.inner.queue.lock().unwrap();
        let empty = q.is_empty();
        drop(q);
        empty && *self.inner.closed.lock().unwrap()
    }
}
