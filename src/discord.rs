use anyhow::{Result, bail};
use serde_json::{Value, json};
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

#[cfg(windows)]
type Stream = tokio::net::windows::named_pipe::NamedPipeClient;
#[cfg(unix)]
type Stream = tokio::net::UnixStream;

pub struct Discord {
    stream: Option<Stream>,
    app_id: &'static str,
}

impl Discord {
    pub fn new() -> Self {
        Self {
            stream: None,
            app_id: "1434412611411120198",
        }
    }

    async fn connect(&mut self) -> Result<()> {
        for index in 0..10 {
            #[cfg(windows)]
            let connection = tokio::net::windows::named_pipe::ClientOptions::new()
                .open(format!(r"\\.\pipe\discord-ipc-{index}"));
            #[cfg(unix)]
            let connection = {
                let base = std::env::var_os("XDG_RUNTIME_DIR")
                    .map(std::path::PathBuf::from)
                    .unwrap_or_else(std::env::temp_dir);
                tokio::net::UnixStream::connect(base.join(format!("discord-ipc-{index}"))).await
            };
            if let Ok(stream) = connection {
                self.stream = Some(stream);
                self.exchange(0, json!({"v": 1, "client_id": self.app_id}))
                    .await?;
                return Ok(());
            }
        }
        bail!("Discord desktop não encontrado")
    }

    async fn exchange(&mut self, code: u32, body: Value) -> Result<()> {
        let stream = self
            .stream
            .as_mut()
            .ok_or_else(|| anyhow::anyhow!("Discord desconectado"))?;
        tokio::time::timeout(Duration::from_secs(2), async {
            let bytes = serde_json::to_vec(&body)?;
            stream.write_u32_le(code).await?;
            stream.write_u32_le(bytes.len() as u32).await?;
            stream.write_all(&bytes).await?;
            let opcode = stream.read_u32_le().await?;
            let length = stream.read_u32_le().await?;
            if length > 1024 * 1024 {
                bail!("resposta Discord excede limite");
            }
            let mut bytes = vec![0; length as usize];
            stream.read_exact(&mut bytes).await?;
            if opcode == 2 {
                bail!("Discord fechou conexão");
            }
            let response: Value = serde_json::from_slice(&bytes)?;
            if response["evt"] == "ERROR" {
                bail!("Discord recusou atividade");
            }
            Ok(())
        })
        .await?
    }

    pub async fn activity(&mut self, activity: Value) -> Result<()> {
        if self.stream.is_none() {
            self.connect().await?;
        }
        let result = self.exchange(1, json!({"cmd":"SET_ACTIVITY", "nonce":crate::auth::secret(), "args":{"pid":std::process::id(), "activity":activity}})).await;
        if result.is_err() {
            self.stream = None;
        }
        result
    }
}

pub fn watching(
    title: &str,
    episode: u32,
    position: f64,
    duration: f64,
    paused: bool,
    cover: &str,
) -> Value {
    let mut activity = json!({"details": title, "state": if episode > 0 { format!("Episódio {episode}{}", if paused { " · Pausado" } else { "" }) } else { "Assistindo".into() }, "type":3, "assets":{"large_image":if cover.is_empty() { "mpv" } else { cover }, "large_text": title}});
    if !paused
        && duration.is_finite()
        && position.is_finite()
        && duration > position
        && position >= 0.0
    {
        let now = crate::runtime::unix_now() as f64;
        activity["timestamps"] =
            json!({"start": (now-position) as i64, "end": (now+duration-position) as i64});
    }
    activity
}
