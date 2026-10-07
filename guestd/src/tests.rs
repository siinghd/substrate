use crate::protocol::*;
use std::io::Cursor;
use std::time::Duration;

const GOLDEN_HEX: &str = "0100000001020b0000000000007b2270726f746f223a317d030000000103100000000000007b22657865635f6964223a226531227d";

#[test]
fn golden_vector() {
    let raw = hex_decode(GOLDEN_HEX);
    let mut r = Cursor::new(&raw[..]);
    let f1 = read_frame(&mut r).unwrap();
    assert_eq!(f1.stream_id, 1);
    assert_eq!(f1.op, OP_HELLO);
    assert_eq!(&f1.payload, b"{\"proto\":1}");
    let f2 = read_frame(&mut r).unwrap();
    assert_eq!(f2.stream_id, 3);
    assert_eq!(f2.op, OP_EXEC_REQ);
    assert_eq!(&f2.payload, b"{\"exec_id\":\"e1\"}");

    let mut out = Vec::new();
    write_frame(&mut out, &Frame { stream_id: 1, flags: FLAG_SYN, op: OP_HELLO, payload: b"{\"proto\":1}".to_vec() }).unwrap();
    write_frame(&mut out, &Frame { stream_id: 3, flags: FLAG_SYN, op: OP_EXEC_REQ, payload: b"{\"exec_id\":\"e1\"}".to_vec() }).unwrap();
    assert_eq!(hex_encode(&out), GOLDEN_HEX);
}

#[test]
fn frame_roundtrip_max_size() {
    let big = vec![0xABu8; MAX_FRAME_SIZE as usize];
    let f = Frame { stream_id: 9, flags: FLAG_BINARY, op: OP_EXEC_DATA, payload: big.clone() };
    let mut buf = Vec::new();
    write_frame(&mut buf, &f).unwrap();
    let got = read_frame(&mut Cursor::new(buf)).unwrap();
    assert_eq!(got.payload, big);

    let oversized = Frame { stream_id: 1, flags: 0, op: 0, payload: vec![0; (MAX_FRAME_SIZE + 1) as usize] };
    assert!(write_frame(&mut Vec::new(), &oversized).is_err());
}

fn hex_decode(s: &str) -> Vec<u8> {
    (0..s.len()).step_by(2).map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap()).collect()
}

fn hex_encode(b: &[u8]) -> String {
    b.iter().map(|x| format!("{:02x}", x)).collect()
}

fn socket_pair() -> (std::os::unix::net::UnixStream, std::os::unix::net::UnixStream) {
    std::os::unix::net::UnixStream::pair().unwrap()
}

#[test]
fn mux_ping_pong() {
    let (a, b) = socket_pair();
    let a2 = a.try_clone().unwrap();
    let b2 = b.try_clone().unwrap();
    let host = Mux::new(Box::new(a), Box::new(a2), true);
    let guest = Mux::new(Box::new(b), Box::new(b2), false);

    let st = host.open(OP_PING, vec![42]).unwrap();
    let got = guest.accept().unwrap();
    let f = got.recv().unwrap();
    assert_eq!(f.op, OP_PING);
    assert_eq!(f.payload, vec![42]);
    got.send_fin(OP_PONG, vec![43]).unwrap();

    let back = st.recv().unwrap();
    assert_eq!(back.payload, vec![43]);
}

#[test]
fn mux_many_streams() {
    let (a, b) = socket_pair();
    let a2 = a.try_clone().unwrap();
    let b2 = b.try_clone().unwrap();
    let host = Mux::new(Box::new(a), Box::new(a2), true);
    let guest = Mux::new(Box::new(b), Box::new(b2), false);

    let g = guest.clone();
    std::thread::spawn(move || {
        for _ in 0..20 {
            if let Some(s) = g.accept() {
                let f = s.recv().unwrap();
                s.send_fin(OP_PONG, f.payload).unwrap();
            }
        }
    });

    for i in 0..20u32 {
        let st = host.open(OP_PING, i.to_le_bytes().to_vec()).unwrap();
        let f = st.recv().unwrap();
        assert_eq!(f.payload, i.to_le_bytes().to_vec());
    }
}

/// Regression: `recv()` popped a frame without notifying the condvar, so a
/// producer parked because the queue was full never woke — even after the
/// queue drained. The producer is `read_loop`, the ONE thread reading the
/// vsock, so a single slow consumer wedged every stream on the sandbox.
///
/// Drives more than QUEUE_CAP frames through one stream while the consumer
/// reads slowly. Before the fix this hangs; the test asserts it does not.
#[test]
fn mux_slow_consumer_does_not_wedge_read_loop() {
    let (a, b) = socket_pair();
    let a2 = a.try_clone().unwrap();
    let b2 = b.try_clone().unwrap();
    let host = Mux::new(Box::new(a), Box::new(a2), true);
    let guest = Mux::new(Box::new(b), Box::new(b2), false);

    // Well past QUEUE_CAP so the producer must block at least once.
    const N: usize = 700;

    let h = host.clone();
    std::thread::spawn(move || {
        let st = h.open(OP_EXEC_DATA, vec![0]).unwrap();
        for i in 0..N {
            let _ = st.send(OP_EXEC_DATA, vec![(i % 251) as u8]);
        }
        let _ = st.send_fin(OP_EXEC_DATA, vec![]);
    });

    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let s = match guest.accept() {
            Some(s) => s,
            None => {
                let _ = tx.send(0usize);
                return;
            }
        };
        // Be genuinely slow to start: the producer must reach QUEUE_CAP and
        // park in push() before this thread pops anything. Without the stall
        // the queue never fills and the lost wakeup is unreachable.
        std::thread::sleep(Duration::from_millis(300));
        let mut seen = 0usize;
        // The SYN frame counts as the first.
        while let Some(f) = s.recv_timeout(Duration::from_secs(5)) {
            seen += 1;
            if f.has(FLAG_FIN) {
                break;
            }
        }
        let _ = tx.send(seen);
    });

    match rx.recv_timeout(Duration::from_secs(20)) {
        Ok(seen) => {
            assert!(
                seen >= N,
                "consumer saw only {seen} of {N} frames: the demux thread stalled"
            );
        }
        Err(_) => panic!("read_loop wedged: a full queue parked the single vsock reader forever"),
    }
}

/// Regression: two halves of the same leak.
///
/// `close()` was a purely local map removal that put nothing on the wire, and
/// `read_loop` never reclaimed an entry on FIN. Either alone leaks; together
/// the peer's stream table grew for the life of the connection — an unbounded
/// map inside a 512MiB guest.
///
/// The accepting side deliberately never calls `close()`, so its table can
/// only drain if the initiator's close reaches it as a FIN and read_loop acts
/// on it.
#[test]
fn mux_reclaims_stream_table_on_fin() {
    let (a, b) = socket_pair();
    let a2 = a.try_clone().unwrap();
    let b2 = b.try_clone().unwrap();
    let host = Mux::new(Box::new(a), Box::new(a2), true);
    let guest = Mux::new(Box::new(b), Box::new(b2), false);

    let g = guest.clone();
    std::thread::spawn(move || {
        while let Some(s) = g.accept() {
            // Answers so the initiator can proceed, but never closes.
            if s.recv().is_some() {
                let _ = s.send(OP_PONG, vec![]);
            }
        }
    });

    const ROUNDS: u32 = 200;
    for i in 0..ROUNDS {
        let st = host.open(OP_PING, i.to_le_bytes().to_vec()).unwrap();
        assert!(
            st.recv_timeout(Duration::from_secs(5)).is_some(),
            "round {i}: peer never answered"
        );
        st.close();
    }

    let deadline = std::time::Instant::now() + Duration::from_secs(5);
    loop {
        let n = guest.live_stream_count();
        if n < 32 {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "peer stream table still holds {n} entries after {ROUNDS} closed streams: close() is not reaching the peer, or FIN is not reclaiming it"
        );
        std::thread::sleep(Duration::from_millis(20));
    }
}

mod agent_tests {
    use super::*;

    #[test]
    fn fs_ops_roundtrip() {
        let dir = std::env::temp_dir().join(format!("guestd-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let file = dir.join("hello.txt");
        std::fs::write(&file, b"hello world").unwrap();

        let req = crate::messages::FsRequest {
            op: "read".into(),
            path: file.to_string_lossy().into_owned(),
            offset: 0,
            length: 0,
            mode: 0,
            append: false,
        };
        let frame = Frame {
            stream_id: 7,
            flags: FLAG_SYN,
            op: OP_FS_REQ,
            payload: serde_json::to_vec(&req).unwrap(),
        };

        // drive handle() over an in-memory stream pair via the mux
        let (a, b) = socket_pair();
        let a2 = a.try_clone().unwrap();
        let b2 = b.try_clone().unwrap();
        let host = Mux::new(Box::new(a), Box::new(a2), true);
        let guest = Mux::new(Box::new(b), Box::new(b2), false);

        let st = host.open(OP_FS_REQ, frame.payload.clone()).unwrap();
        let gst = guest.accept().unwrap();
        let first = gst.recv().unwrap();
        crate::fsops::handle(gst, first);

        let mut data = Vec::new();
        loop {
            match st.recv() {
                Some(f) if f.op == OP_FS_DATA && f.has(FLAG_BINARY) => data.extend_from_slice(&f.payload),
                Some(f) if f.op == OP_FS_RESP => {
                    let resp: crate::messages::FsResponse = serde_json::from_slice(&f.payload).unwrap();
                    assert!(resp.ok);
                    break;
                }
                None => panic!("stream closed early"),
                _ => {}
            }
        }
        assert_eq!(data, b"hello world");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn sysinfo_parses() {
        let m = crate::sysinfo::collect();
        assert!(m.mem_total_kb > 0);
        assert!(m.uptime_s > 0);
        let ports = crate::sysinfo::listening_ports();
        let _ = ports;
    }
}
