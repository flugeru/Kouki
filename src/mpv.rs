use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{process::Stdio, time::Duration};
use tokio::{
    io::{AsyncBufReadExt, AsyncRead, AsyncWrite, AsyncWriteExt, BufReader},
    process::{Child, Command},
};

trait Transport: AsyncRead + AsyncWrite + Unpin + Send {}
impl<T: AsyncRead + AsyncWrite + Unpin + Send> Transport for T {}

pub struct Ipc {
    stream: BufReader<Box<dyn Transport>>,
    next_id: u64,
}

#[derive(Clone, Default, Debug, Serialize, Deserialize)]
pub struct Playback {
    pub path: String,
    pub title: String,
    pub position: f64,
    pub duration: f64,
    pub paused: bool,
}

impl Ipc {
    pub async fn connect(endpoint: &str) -> Result<Self> {
        #[cfg(windows)]
        let stream = tokio::net::windows::named_pipe::ClientOptions::new().open(endpoint)?;
        #[cfg(unix)]
        let stream = tokio::time::timeout(
            Duration::from_secs(2),
            tokio::net::UnixStream::connect(endpoint),
        )
        .await??;
        Ok(Self {
            stream: BufReader::new(Box::new(stream)),
            next_id: 0,
        })
    }

    pub async fn command(&mut self, args: Value) -> Result<Value> {
        self.next_id += 1;
        let id = self.next_id;
        let message = format!("{}\n", json!({"command": args, "request_id": id}));
        tokio::time::timeout(Duration::from_secs(2), async {
            self.stream.get_mut().write_all(message.as_bytes()).await?;
            self.stream.get_mut().flush().await?;
            loop {
                let mut line = Vec::new();
                // Limit before allocation growth; MPV events and replies share stream.
                loop {
                    let buf = self.stream.fill_buf().await?;
                    if buf.is_empty() {
                        bail!("MPV desconectado");
                    }
                    let count = buf
                        .iter()
                        .position(|b| *b == b'\n')
                        .map_or(buf.len(), |i| i + 1);
                    if line.len() + count > 1024 * 1024 {
                        bail!("resposta MPV excede 1 MiB");
                    }
                    line.extend_from_slice(&buf[..count]);
                    self.stream.consume(count);
                    if line.last() == Some(&b'\n') {
                        break;
                    }
                }
                let response: Value = serde_json::from_slice(&line)?;
                if response["request_id"].as_u64() != Some(id) {
                    continue;
                }
                if response["error"] != "success" {
                    bail!("MPV: {}", response["error"]);
                }
                return Ok(response["data"].clone());
            }
        })
        .await
        .context("tempo esgotado no IPC MPV")?
    }

    pub async fn playback(&mut self) -> Result<Playback> {
        let get = |property: &'static str| json!(["get_property", property]);
        let path = self
            .command(get("path"))
            .await?
            .as_str()
            .unwrap_or_default()
            .to_owned();
        let title = self
            .command(get("media-title"))
            .await?
            .as_str()
            .unwrap_or_default()
            .to_owned();
        let position = self
            .command(get("time-pos"))
            .await?
            .as_f64()
            .unwrap_or_default();
        let duration = self
            .command(get("duration"))
            .await?
            .as_f64()
            .unwrap_or_default();
        let paused = self.command(get("pause")).await?.as_bool().unwrap_or(true);
        Ok(Playback {
            path,
            title,
            position,
            duration,
            paused,
        })
    }
}

pub async fn launch(executable: &str, endpoint: &str, file: &str) -> Result<Child> {
    let mut cmd = Command::new(executable);
    cmd.args([
        "--idle=yes",
        "--force-window=yes",
        &format!("--input-ipc-server={endpoint}"),
    ]);
    // End option parsing: media beginning with '-' cannot inject MPV options.
    if !file.is_empty() {
        cmd.arg("--").arg(file);
    }
    cmd.stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .kill_on_drop(true);
    cmd.spawn()
        .context("não foi possível iniciar MPV; confira caminho nas configurações")
}

#[cfg(all(test, windows))]
mod windows_tests {
    use super::*;
    use tokio::net::windows::named_pipe::ServerOptions;

    #[tokio::test]
    async fn named_pipe_matches_response_and_ignores_events() {
        let name = format!(r"\\.\pipe\kouki-test-{}", crate::auth::secret());
        let server = ServerOptions::new()
            .first_pipe_instance(true)
            .create(&name)
            .unwrap();
        let task = tokio::spawn(async move {
            server.connect().await.unwrap();
            let mut reader = BufReader::new(server);
            let mut request = String::new();
            reader.read_line(&mut request).await.unwrap();
            assert_eq!(
                serde_json::from_str::<Value>(&request).unwrap()["request_id"],
                1
            );
            reader
                .get_mut()
                .write_all(
                    b"{\"event\":\"idle\"}\n{\"request_id\":1,\"error\":\"success\",\"data\":42}\n",
                )
                .await
                .unwrap();
        });
        let mut ipc = Ipc::connect(&name).await.unwrap();
        assert_eq!(
            ipc.command(json!(["get_property", "time-pos"]))
                .await
                .unwrap(),
            42
        );
        task.await.unwrap();
    }

    #[tokio::test]
    #[ignore = "requires KOUKI_TEST_MPV pointing to installed mpv.exe"]
    async fn real_mpv_ipc_smoke() {
        let executable = std::env::var("KOUKI_TEST_MPV").expect("set KOUKI_TEST_MPV");
        let endpoint = format!(r"\\.\pipe\kouki-smoke-{}", crate::auth::secret());
        // Test never loads or changes user's configuration; no visible window.
        let mut child = Command::new(executable)
            .args([
                "--no-config",
                "--idle=yes",
                "--vo=null",
                "--ao=null",
                &format!("--input-ipc-server={endpoint}"),
            ])
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .unwrap();
        let mut ipc = tokio::time::timeout(Duration::from_secs(10), async {
            loop {
                if let Ok(ipc) = Ipc::connect(&endpoint).await {
                    return ipc;
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
        })
        .await
        .unwrap();
        let version = ipc
            .command(json!(["get_property", "mpv-version"]))
            .await
            .unwrap();
        assert!(version.as_str().unwrap().starts_with("mpv "));
        ipc.command(json!(["set_property", "pause", true]))
            .await
            .unwrap();
        assert_eq!(
            ipc.command(json!(["get_property", "pause"])).await.unwrap(),
            true
        );
        child.kill().await.unwrap();
        child.wait().await.unwrap();
    }
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    #[tokio::test]
    async fn ignores_events_and_matches_request_id() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ipc");
        let listener = tokio::net::UnixListener::bind(&path).unwrap();
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut reader = BufReader::new(stream);
            let mut request = String::new();
            reader.read_line(&mut request).await.unwrap();
            assert_eq!(
                serde_json::from_str::<Value>(&request).unwrap()["request_id"],
                1
            );
            reader
                .get_mut()
                .write_all(
                    b"{\"event\":\"idle\"}\n{\"request_id\":1,\"error\":\"success\",\"data\":42}\n",
                )
                .await
                .unwrap();
        });
        let mut ipc = Ipc::connect(path.to_str().unwrap()).await.unwrap();
        assert_eq!(
            ipc.command(json!(["get_property", "time-pos"]))
                .await
                .unwrap(),
            42
        );
        server.await.unwrap();
    }
}
