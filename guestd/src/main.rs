mod exec;
mod fsops;
mod messages;
mod protocol;
mod reseed;
mod sysinfo;
#[cfg(test)]
mod tests;

use messages::*;
use protocol::*;
use std::io::{Read, Write};
use std::sync::Arc;

const VSOCK_PORT: u32 = 5000;
const PROTO_VERSION: i32 = 1;

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let mut unix_path: Option<String> = None;
    let mut vsock_port = VSOCK_PORT;
    let mut i = 1;
    while i < args.len() {
        match args[i].as_str() {
            "--unix" => {
                unix_path = Some(args.get(i + 1).cloned().unwrap_or_default());
                i += 2;
            }
            "--vsock-port" => {
                vsock_port = args.get(i + 1).and_then(|s| s.parse().ok()).unwrap_or(VSOCK_PORT);
                i += 2;
            }
            other => {
                eprintln!("guestd: unknown arg {}", other);
                std::process::exit(2);
            }
        }
    }

    // LISTEN, don't dial. The transport contract (docs/protocol.md) is that
    // the HOST dials the guest (vsock CID:5000 / unix socket); the Go twin
    // (cmd/shim) listens the same way. The previous dial-out loop opened two
    // unsolicited connections to a host listener nobody implements — against
    // noded both ends dialed and neither accepted, so the hv data path could
    // never come up.
    match &unix_path {
        Some(path) => {
            let _ = std::fs::remove_file(path);
            let ln = match std::os::unix::net::UnixListener::bind(path) {
                Ok(l) => l,
                Err(e) => {
                    eprintln!("guestd: bind {}: {}", path, e);
                    std::process::exit(1);
                }
            };
            // Agent socket is host-controlled; keep it private in the jail.
            let _ = std::fs::set_permissions(path, std::os::unix::fs::PermissionsExt::from_mode(0o600));
            for conn in ln.incoming() {
                match conn {
                    Ok(s) => {
                        let r: Box<dyn Read + Send> = Box::new(s.try_clone().expect("dup sock"));
                        let w: Box<dyn Write + Send> = Box::new(s);
                        serve(r, w);
                    }
                    Err(e) => eprintln!("guestd: accept: {}", e),
                }
            }
        }
        None => {
            // Bind ANY local CID: in the guest this accepts the host's
            // connection to our CID (the host dials guest_cid:5000).
            let ln = match vsock::VsockListener::bind_with_cid_port(vsock::VMADDR_CID_ANY, vsock_port as u32) {
                Ok(l) => l,
                Err(e) => {
                    eprintln!("guestd: vsock bind :{}: {}", vsock_port, e);
                    std::process::exit(1);
                }
            };
            for conn in ln.incoming() {
                match conn {
                    Ok(s) => {
                        let r: Box<dyn Read + Send> = Box::new(match s.try_clone() {
                            Ok(c) => c,
                            Err(_) => continue,
                        });
                        let w: Box<dyn Write + Send> = Box::new(s);
                        serve(r, w);
                    }
                    Err(e) => eprintln!("guestd: vsock accept: {}", e),
                }
            }
        }
    }
}

fn serve(r: Box<dyn Read + Send>, w: Box<dyn Write + Send>) {
    let mux = Mux::new(r, w, false);
    let mgr = Arc::new(exec::ExecManager::new());

    announce_hello(&mux);
    announce_ports(&mux);

    let m2 = mux.clone();
    std::thread::spawn(move || announcer_loop(m2));

    while let Some(stream) = mux.accept() {
        let first = match stream.recv() {
            Some(f) => f,
            None => continue,
        };
        let mgr2 = mgr.clone();
        match first.op {
            OP_EXEC_REQ => {
                std::thread::spawn(move || exec::handle(&mgr2, stream, first));
            }
            OP_FS_REQ => {
                std::thread::spawn(move || fsops::handle(stream, first));
            }
            OP_RESEED => {
                if let Ok(rs) = serde_json::from_slice::<Reseed>(&first.payload) {
                    let reply = match reseed::reseed(&rs.vmgenid) {
                        Ok((machine_id, fp)) => ReseedAck { machine_id, ssh_fingerprint: fp },
                        Err(e) => ReseedAck { machine_id: format!("error:{}", e), ssh_fingerprint: String::new() },
                    };
                    let _ = stream.send_fin(OP_RESEED_ACK, serde_json::to_vec(&reply).unwrap());
                } else {
                    let _ = stream.close();
                }
            }
            OP_PING => {
                let _ = stream.send_fin(OP_PONG, vec![]);
            }
            OP_EXEC_SIGNAL => {
                if let Ok(sig) = serde_json::from_slice::<exec::SignalMsg>(&first.payload) {
                    mgr2.kill(&sig.exec_id, &sig.signal);
                }
                let _ = stream.close();
            }
            _ => {
                let _ = stream.close();
            }
        }
    }
}

fn announce_hello(mux: &Arc<Mux>) {
    let hello = Hello {
        proto: PROTO_VERSION,
        agent: "guestd".into(),
        version: env!("CARGO_PKG_VERSION").into(),
        boot_id: read_boot_id(),
        features: vec!["exec".into(), "fs".into(), "pty".into(), "reseed".into()],
    };
    if let Ok(st) = mux.open(OP_HELLO, serde_json::to_vec(&hello).unwrap()) {
        st.close();
    }
}

fn read_boot_id() -> String {
    std::fs::read_to_string("/proc/sys/kernel/random/boot_id")
        .map(|s| s.trim().to_string())
        .unwrap_or_default()
}

fn announce_ports(mux: &Arc<Mux>) {
    let msg = PortsAnnounce {
        ports: sysinfo::listening_ports()
            .into_iter()
            .map(|p| PortEntry { port: p, proto: "tcp".into() })
            .collect(),
    };
    if let Ok(st) = mux.open(OP_PORTS, serde_json::to_vec(&msg).unwrap()) {
        st.close();
    }
}


fn announcer_loop(mux: Arc<Mux>) {
    let mut tick: u64 = 0;
    loop {
        std::thread::sleep(std::time::Duration::from_secs(15));
        if mux.is_dead() {
            return;
        }
        tick += 1;
        let m = sysinfo::collect();
        let msg = GuestMetrics {
            mem_total_kb: m.mem_total_kb,
            mem_avail_kb: m.mem_avail_kb,
            load1: m.load1,
            pids: m.pids,
            uptime_s: m.uptime_s,
        };
        if let Ok(st) = mux.open(OP_METRICS, serde_json::to_vec(&msg).unwrap()) {
            st.close();
        }
        if tick % 4 == 0 {
            announce_ports(&mux);
        }
    }
}


