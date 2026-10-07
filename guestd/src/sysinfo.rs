use std::fs;

fn read_first_number(path: &str, key: &str) -> Option<u64> {
    let content = fs::read_to_string(path).ok()?;
    for line in content.lines() {
        if let Some(rest) = line.strip_prefix(key) {
            let num: String = rest.chars().skip_while(|c| !c.is_ascii_digit()).take_while(|c| c.is_ascii_digit()).collect();
            return num.parse().ok();
        }
    }
    None
}

pub struct Metrics {
    pub mem_total_kb: u64,
    pub mem_avail_kb: u64,
    pub load1: u32,
    pub pids: u32,
    pub uptime_s: u64,
}

pub fn collect() -> Metrics {
    let mem_total_kb = read_first_number("/proc/meminfo", "MemTotal:").unwrap_or(0);
    let mem_avail_kb = read_first_number("/proc/meminfo", "MemAvailable:").unwrap_or(0);
    let load1 = fs::read_to_string("/proc/loadavg")
        .ok()
        .and_then(|s| s.split_whitespace().next().and_then(|f| f.split('.').next().and_then(|i| i.parse().ok())))
        .unwrap_or(0);
    let uptime_s = fs::read_to_string("/proc/uptime")
        .ok()
        .and_then(|s| s.split_whitespace().next().and_then(|f| f.split('.').next().and_then(|i| i.parse().ok())))
        .unwrap_or(0);
    let pids = fs::read_dir("/proc")
        .map(|rd| rd.flatten().filter(|e| e.file_name().to_string_lossy().chars().all(|c| c.is_ascii_digit())).count() as u32)
        .unwrap_or(0);
    Metrics { mem_total_kb, mem_avail_kb, load1, pids, uptime_s }
}

/// Listening TCP ports parsed from /proc/net/tcp{,6} (st == 0A means LISTEN).
pub fn listening_ports() -> Vec<u16> {
    let mut ports = Vec::new();
    for path in ["/proc/net/tcp", "/proc/net/tcp6"] {
        let content = match fs::read_to_string(path) {
            Ok(c) => c,
            Err(_) => continue,
        };
        for line in content.lines().skip(1) {
            let cols: Vec<&str> = line.split_whitespace().collect();
            if cols.len() < 4 {
                continue;
            }
            if cols[3] != "0A" {
                continue;
            }
            if let Some(port_hex) = cols[1].split(':').nth(1) {
                if let Ok(p) = u16::from_str_radix(port_hex, 16) {
                    if p != 0 && !ports.contains(&p) {
                        ports.push(p);
                    }
                }
            }
        }
    }
    ports.sort_unstable();
    ports
}
