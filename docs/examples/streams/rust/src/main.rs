//! Logs from eRPC's StreamLogs on the bds crate, applied to a store keyed by
//! (blockHash, logIndex) and resumed from erpc.cursor, with the toolkit rules
//! done by hand: windows, heartbeat watchdog, reconnect, undo, out of range.
//! Exits after BLOCKS blocks (default: never).
//!
//!     ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 ERPC_ADDRESS=0x… cargo run

use std::{collections::HashMap, env, error::Error, fs, io, time::Duration, time::Instant};

use bds::evm::{
    stream_logs_response::Message, stream_service_client::StreamServiceClient, stream_status::State, BlockRef,
    Finality, Log, LogFilter, Reorg, StreamLogsRequest,
};
use bytes::Bytes;
use rand::Rng;
use tonic::{
    transport::{ClientTlsConfig, Endpoint},
    Code, Request, Status,
};
use tonic_types::StatusExt;

/// Seconds; the watchdog reconnects after 3 silent ones.
const HEARTBEAT: u32 = 15;
const CURSOR_PATH: &str = "erpc.cursor";

/// A parsed grpc(s)://host:port/<project>/evm/<chainId>.
struct Target {
    uri: String,
    tls: bool,
    project: String,
    chain: String,
}

fn parse_endpoint(raw: &str) -> Result<Target, String> {
    let bad = |why: &str| format!("{raw}: {why}");
    let (scheme, rest) = raw.split_once("://").ok_or_else(|| bad("no scheme"))?;
    let tls = match scheme {
        "grpc" => false,
        "grpcs" => true,
        _ => return Err(bad("scheme must be grpc or grpcs")),
    };
    let (authority, path) = rest.split_once('/').ok_or_else(|| bad("no path"))?;
    let parts: Vec<&str> = path.trim_matches('/').split('/').collect();
    if parts.len() != 3 || parts[1] != "evm" {
        return Err(bad("path must be /<project>/evm/<chainId>"));
    }
    let authority = match authority.contains(':') {
        true => authority.to_string(),
        false => format!("{authority}:{}", if tls { 443 } else { 80 }),
    };
    let http = if tls { "https" } else { "http" };
    Ok(Target { uri: format!("{http}://{authority}"), tls, project: parts[0].into(), chain: parts[2].into() })
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex(s: &str) -> Result<Vec<u8>, String> {
    let s = s.trim_start_matches("0x");
    (0..s.len())
        .step_by(2)
        .map(|i| s.get(i..i + 2).and_then(|d| u8::from_str_radix(d, 16).ok()).ok_or(format!("bad hex {s}")))
        .collect()
}

fn token(number: u64, hash: &[u8]) -> String {
    format!("0x{number:x}:0x{}", hex(hash))
}

/// The app: its logs and the cursor of the last block it applied.
struct App {
    cursor: Option<(u64, Bytes)>,
    logs: HashMap<(Bytes, u32), (u64, Bytes)>,
    blocks: usize,
    limit: usize,
}

impl App {
    fn load(limit: usize) -> Result<App, Box<dyn Error>> {
        let cursor = match fs::read_to_string(CURSOR_PATH) {
            Ok(s) => {
                let (num, hash) = s.trim().split_once(':').ok_or("bad cursor")?;
                Some((u64::from_str_radix(num.trim_start_matches("0x"), 16)?, Bytes::from(unhex(hash)?)))
            }
            Err(e) if e.kind() == io::ErrorKind::NotFound => None,
            Err(e) => return Err(e.into()),
        };
        Ok(App { cursor, logs: HashMap::new(), blocks: 0, limit })
    }

    /// Writes a temporary file, syncs it and renames it over the cursor, so a
    /// crash leaves the old token or the new one.
    fn save(&mut self, number: u64, hash: Bytes) -> io::Result<()> {
        let tmp = format!("{CURSOR_PATH}.tmp");
        let f = fs::File::create(&tmp)?;
        io::Write::write_all(&mut &f, format!("{}\n", token(number, &hash)).as_bytes())?;
        f.sync_all()?;
        fs::rename(&tmp, CURSOR_PATH)?;
        self.cursor = Some((number, hash));
        Ok(())
    }

    /// Idempotent: a block applied again (a crash before the cursor save)
    /// writes the same keys.
    fn apply(&mut self, block: &BlockRef, logs: Vec<Log>) -> io::Result<()> {
        println!("block {} 0x{}: {} logs", block.number, hex(&block.hash), logs.len());
        for log in logs {
            self.logs.insert((log.block_hash, log.log_index), (block.number, log.data));
        }
        self.blocks += 1;
        self.save(block.number, block.hash.clone())
    }

    fn undo(&mut self, fork_point: &BlockRef) -> io::Result<()> {
        println!("undo to {} 0x{}", fork_point.number, hex(&fork_point.hash));
        self.logs.retain(|_, (n, _)| *n <= fork_point.number);
        self.save(fork_point.number, fork_point.hash.clone())
    }

    fn done(&self) -> bool {
        self.limit > 0 && self.blocks >= self.limit
    }
}

/// How a session ended.
enum End {
    Done,
    Restart,
    Watchdog,
    Closed,
    Failed(Status),
}

/// One stream from the cursor until it ends; true when a message arrived.
async fn session(app: &mut App, t: &Target, filter: &LogFilter, secret: Option<&str>) -> Result<(bool, End), Box<dyn Error>> {
    // The server's windows: with the 64 KiB defaults, log bursts wait on
    // this client's acknowledgements.
    let mut ep = Endpoint::from_shared(t.uri.clone())?
        .initial_stream_window_size(4 << 20)
        .initial_connection_window_size(16 << 20);
    if t.tls {
        ep = ep.tls_config(ClientTlsConfig::new().with_native_roots())?;
    }
    // A fresh channel per session: a black-holed connection is never reused.
    let channel = match ep.connect().await {
        Ok(c) => c,
        Err(e) => return Ok((false, End::Failed(Status::unavailable(e.to_string())))),
    };
    let mut req = Request::new(StreamLogsRequest {
        from_block: app.cursor.as_ref().map(|c| c.0),
        from_hash: app.cursor.as_ref().map(|c| c.1.clone()),
        filter: Some(filter.clone()),
        finality: Finality::Latest as i32,
        reorg: Reorg::Undo as i32,
        heartbeat_seconds: Some(HEARTBEAT),
        ..Default::default()
    });
    req.metadata_mut().insert("x-erpc-project", t.project.parse()?);
    req.metadata_mut().insert("x-erpc-chain-id", t.chain.parse()?);
    if let Some(s) = secret {
        req.metadata_mut().insert("x-erpc-secret-token", s.parse()?);
    }
    let mut stream = match StreamServiceClient::new(channel).stream_logs(req).await {
        Ok(r) => r.into_inner(),
        Err(s) => return Ok((false, End::Failed(s))),
    };
    let watchdog = Duration::from_secs(3 * HEARTBEAT as u64);
    let mut flowed = false;
    let mut part: Option<(BlockRef, Vec<Log>)> = None; // the block in progress
    loop {
        let msg = match tokio::time::timeout(watchdog, stream.message()).await {
            Err(_) => return Ok((flowed, End::Watchdog)),
            Ok(Err(s)) => return Ok((flowed, End::Failed(s))),
            Ok(Ok(None)) => return Ok((flowed, End::Closed)),
            Ok(Ok(Some(m))) => m,
        };
        flowed = true;
        match msg.message {
            Some(Message::Block(bl)) => {
                let Some(r) = bl.block else { continue };
                if part.as_ref().is_none_or(|(p, _)| p.hash != r.hash) {
                    part = Some((r.clone(), Vec::new()));
                }
                if let Some((_, logs)) = part.as_mut() {
                    logs.extend(bl.logs);
                }
                if !bl.complete {
                    continue;
                }
                let Some((r, logs)) = part.take() else { continue };
                match &app.cursor {
                    Some((_, h)) if *h == r.hash => continue, // already applied: skip
                    Some((_, h)) if *h != r.parent_hash => return Ok((flowed, End::Restart)),
                    _ => app.apply(&r, logs)?,
                }
            }
            Some(Message::Undo(u)) => {
                part = None;
                if let Some(fp) = u.fork_point {
                    app.undo(&fp)?;
                }
            }
            Some(Message::Status(st)) => {
                let state = State::try_from(st.state).map_or("unknown", |s| s.as_str_name());
                println!("status {state} {}", st.reason);
            }
            Some(Message::Heartbeat(_)) | None => {}
        }
        if app.done() {
            return Ok((flowed, End::Done));
        }
    }
}

fn out_of_range(s: &Status, cursor: &Option<(u64, Bytes)>) -> String {
    let from = cursor.as_ref().map_or("none".into(), |(n, h)| token(*n, h));
    match s.get_details_error_info() {
        Some(info) => {
            let n: u64 = info.metadata.get("oldestBlock").and_then(|v| v.parse().ok()).unwrap_or(0);
            let h = info.metadata.get("oldestHash").cloned().unwrap_or_default();
            format!("cursor {from} out of range ({}); oldest resumable 0x{n:x}:{h}", info.reason)
        }
        None => format!("cursor {from} out of range: {}", s.message()),
    }
}

async fn run() -> Result<(), Box<dyn Error>> {
    let t = parse_endpoint(&env::var("ERPC_ENDPOINT")?)?;
    let mut filter = LogFilter::default();
    if let Ok(a) = env::var("ERPC_ADDRESS") {
        filter.address.push(Bytes::from(unhex(&a)?));
    }
    let secret = env::var("ERPC_TOKEN").ok();
    let mut app = App::load(env::var("BLOCKS").map_or(0, |b| b.parse().unwrap_or(0)))?;
    let mut attempt = 0;
    loop {
        let began = Instant::now();
        let (flowed, end) = session(&mut app, &t, &filter, secret.as_deref()).await?;
        if began.elapsed() >= Duration::from_secs(60) {
            attempt = 0;
        }
        let at_once = match end {
            End::Done => return Ok(()),
            End::Restart => true, // linkage broke: resume from the cursor
            End::Watchdog | End::Closed => flowed,
            End::Failed(s) => match s.code() {
                Code::OutOfRange => return Err(out_of_range(&s, &app.cursor).into()),
                Code::Aborted | Code::Unauthenticated | Code::PermissionDenied | Code::InvalidArgument => {
                    return Err(s.into())
                }
                // Drained, aged out, stalled or lagging: the cursor loses nothing.
                c => flowed && c == Code::Unavailable,
            },
        };
        if at_once {
            continue;
        }
        let delay = (0.5 * 2f64.powi(attempt.min(6))).min(30.0);
        attempt += 1;
        println!("reconnect in up to {delay}s");
        tokio::time::sleep(Duration::from_secs_f64(delay / 2.0 + rand::rng().random_range(0.0..=delay / 2.0))).await;
    }
}

#[tokio::main]
async fn main() {
    if let Err(e) = run().await {
        // Out of range: away longer than the server's window. Backfill the
        // blocks after the cursor through the oldest resumable block with
        // eth_getLogs, save that block's token to erpc.cursor, then start again.
        eprintln!("{e}");
        std::process::exit(1);
    }
}
