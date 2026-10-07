use serde::{Deserialize, Serialize};

#[derive(Serialize, Deserialize, Clone)]
pub struct Hello {
    pub proto: i32,
    pub agent: String,
    pub version: String,
    pub boot_id: String,
    pub features: Vec<String>,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct ExecRequest {
    pub exec_id: String,
    pub argv: Vec<String>,
    #[serde(default)]
    pub cwd: String,
    #[serde(default)]
    pub env: std::collections::HashMap<String, String>,
    #[serde(default)]
    pub tty: bool,
    #[serde(default)]
    pub rows: u16,
    #[serde(default)]
    pub cols: u16,
    #[serde(default)]
    pub user: String,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct ExecDataMeta {
    pub exec_id: String,
    pub fd: String,
    #[serde(default)]
    pub eof: bool,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct ExecExit {
    pub exec_id: String,
    pub code: i32,
    #[serde(default)]
    pub killed: bool,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct ExecSignal {
    pub exec_id: String,
    pub signal: String,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct ExecResize {
    pub exec_id: String,
    pub rows: u16,
    pub cols: u16,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct FsRequest {
    pub op: String,
    pub path: String,
    #[serde(default)]
    pub offset: i64,
    #[serde(default)]
    pub length: i64,
    #[serde(default)]
    pub mode: u32,
    #[serde(default)]
    pub append: bool,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct FsEntry {
    pub name: String,
    pub size: i64,
    pub dir: bool,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct FsResponse {
    pub ok: bool,
    #[serde(default)]
    pub error: String,
    #[serde(default)]
    pub size: i64,
    #[serde(default)]
    pub entries: Vec<FsEntry>,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct PortEntry {
    pub port: u16,
    pub proto: String,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct PortsAnnounce {
    pub ports: Vec<PortEntry>,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct GuestMetrics {
    pub mem_total_kb: u64,
    pub mem_avail_kb: u64,
    pub load1: u32,
    pub pids: u32,
    pub uptime_s: u64,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct Reseed {
    pub vmgenid: String,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct ReseedAck {
    pub machine_id: String,
    pub ssh_fingerprint: String,
}
