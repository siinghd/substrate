use std::fs;
use std::process::Command;

/// Uniqueness protocol (plan §7.4): after every restore the host sends RESEED
/// with a fresh VMGenID; guestd regenerates every identity the golden snapshot
/// froze: machine-id, SSH host keys, kernel random-seed file.
pub fn reseed(vmgenid: &str) -> Result<(String, String), String> {
    let machine_id = write_machine_id()?;
    let fingerprint = regenerate_ssh_keys().unwrap_or_default();
    clear_random_seed();
    let _ = fs::write("/run/substrate/vmgenid", format!("{}\n", vmgenid));
    Ok((machine_id, fingerprint))
}

fn random_hex(n: usize) -> String {
    let mut buf = vec![0u8; n];
    getrandom(&mut buf);
    buf.iter().map(|b| format!("{:02x}", b)).collect()
}

fn getrandom(buf: &mut [u8]) {
    let r = unsafe { libc::getrandom(buf.as_mut_ptr() as *mut libc::c_void, buf.len(), 0) };
    if r < 0 {
        // fall back to /dev/urandom
        if let Ok(mut f) = fs::File::open("/dev/urandom") {
            use std::io::Read;
            let _ = f.read_exact(buf);
        }
    }
}

fn write_machine_id() -> Result<String, String> {
    let id = random_hex(16);
    fs::write("/etc/machine-id", format!("{}\n", id)).map_err(|e| e.to_string())?;
    if std::path::Path::new("/var/lib/dbus").is_dir() {
        let _ = fs::write("/var/lib/dbus/machine-id", format!("{}\n", id));
    }
    Ok(id)
}

fn regenerate_ssh_keys() -> Option<String> {
    for entry in fs::read_dir("/etc/ssh").ok()?.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if name.starts_with("ssh_host_") && (name.ends_with("_key") || name.ends_with("_key.pub")) {
            let _ = fs::remove_file(entry.path());
        }
    }
    let out = Command::new("ssh-keygen").arg("-A").output().ok()?;
    if !out.status.success() {
        return None;
    }
    let pub_key = "/etc/ssh/ssh_host_ed25519_key.pub";
    let out = Command::new("ssh-keygen").args(["-lf", pub_key]).output().ok()?;
    let line = String::from_utf8_lossy(&out.stdout).into_owned();
    line.split_whitespace().nth(1).map(|s| s.to_string())
}

fn clear_random_seed() {
    // Write FRESH entropy, not an empty file: the point of this step in the
    // uniqueness protocol is that the first boot after restore draws its
    // seed from bytes no other instance of this lineage ever held.
    // Truncating to zero made systemd fall back to warnings + weak seeding.
    let mut seed = vec![0u8; 512];
    getrandom(&mut seed);
    let _ = fs::write("/var/lib/systemd/random-seed", &seed);
}
