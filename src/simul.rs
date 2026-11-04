use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::{sync::Arc, time::Duration};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    net::{TcpListener, TcpStream},
    sync::{Semaphore, watch},
};

#[derive(Clone, Default, Debug, Serialize, Deserialize)]
pub struct State {
    pub title: String,
    pub episode: u32,
    pub anime_id: i64,
    pub episodes: u32,
    pub position: f64,
    pub duration: f64,
    pub paused: bool,
}

fn trusted_address(address: &str) -> Result<std::net::SocketAddr> {
    let address: std::net::SocketAddr = address
        .parse()
        .context("use IP literal e porta, por exemplo 192.168.1.10:45130")?;
    let trusted = match address.ip() {
        std::net::IpAddr::V4(ip) => ip.is_loopback() || ip.is_private(),
        std::net::IpAddr::V6(ip) => ip.is_loopback() || (ip.segments()[0] & 0xfe00 == 0xfc00),
    };
    if !trusted || address.port() == 0 {
        bail!("Simulwatching sem TLS aceita apenas IP privado/loopback e porta não zero");
    }
    Ok(address)
}

async fn line<R: tokio::io::AsyncBufRead + Unpin>(reader: &mut R) -> Result<String> {
    let mut out = Vec::new();
    loop {
        let buf = reader.fill_buf().await?;
        if buf.is_empty() {
            bail!("sessão encerrada");
        }
        let count = buf
            .iter()
            .position(|b| *b == b'\n')
            .map_or(buf.len(), |n| n + 1);
        if out.len() + count > 16 * 1024 {
            bail!("mensagem Simulwatching excede limite");
        }
        out.extend_from_slice(&buf[..count]);
        reader.consume(count);
        if out.last() == Some(&b'\n') {
            return Ok(String::from_utf8(out)?);
        }
    }
}

pub async fn host(
    address: &str,
    code: String,
    state: watch::Receiver<State>,
) -> Result<tokio::task::JoinHandle<()>> {
    // Public exposure is explicit. No automatic router changes and no weak passwords.
    if code.len() < 32 {
        bail!("use convite gerado pelo Kouki (mínimo 32 caracteres)");
    }
    let listener = TcpListener::bind(trusted_address(address)?).await?;
    let slots = Arc::new(Semaphore::new(8));
    Ok(tokio::spawn(async move {
        let mut clients = tokio::task::JoinSet::new();
        loop {
            tokio::select! {
                accepted = listener.accept() => {
                    let Ok((stream, _)) = accepted else { break };
                    let Ok(permit) = slots.clone().try_acquire_owned() else { continue };
                    let code = code.clone();
                    let mut state = state.clone();
                    clients.spawn(async move {
                        let _permit = permit;
                        let authenticated = tokio::time::timeout(Duration::from_secs(10), async {
                            let mut reader = BufReader::new(stream);
                            let hello = line(&mut reader).await?;
                            if hello.trim() != code { bail!("convite inválido"); }
                            Ok::<_, anyhow::Error>(reader)
                        }).await;
                            let Ok(Ok(mut reader)) = authenticated else { return };
                            loop {
                                let bytes = match serde_json::to_vec(&*state.borrow_and_update()) { Ok(bytes) => bytes, Err(_) => break };
                                let write = async {
                                    reader.get_mut().write_all(&bytes).await?;
                                    reader.get_mut().write_all(b"\n").await
                                };
                                if !matches!(tokio::time::timeout(Duration::from_secs(2), write).await, Ok(Ok(()))) { break; }
                                if state.changed().await.is_err() { break; }
                            }
                    });
                }
                _ = clients.join_next(), if !clients.is_empty() => {}
            }
        }
        // JoinSet drop aborts every client when host task is stopped.
    }))
}

pub async fn join(
    address: &str,
    code: &str,
    output: watch::Sender<Option<State>>,
) -> Result<tokio::task::JoinHandle<()>> {
    if code.len() < 32 || code.len() > 128 || code.contains(['\r', '\n']) {
        bail!("convite inválido");
    }
    let mut stream = tokio::time::timeout(
        Duration::from_secs(5),
        TcpStream::connect(trusted_address(address)?),
    )
    .await??;
    stream.write_all(format!("{code}\n").as_bytes()).await?;
    Ok(tokio::spawn(async move {
        let mut reader = BufReader::new(stream);
        loop {
            let state = async {
                let text = line(&mut reader).await?;
                let state: State = serde_json::from_str(&text).context("estado inválido")?;
                if !state.position.is_finite()
                    || !state.duration.is_finite()
                    || state.position < 0.0
                    || state.duration < 0.0
                    || state.title.len() > 1024
                {
                    bail!("estado fora dos limites");
                }
                Ok::<_, anyhow::Error>(state)
            };
            match tokio::time::timeout(Duration::from_secs(15), state).await {
                Ok(Ok(state)) => {
                    let _ = output.send(Some(state));
                }
                _ => break,
            }
        }
        let _ = output.send(None);
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn bounded_messages() {
        let data = vec![b'x'; 16 * 1024 + 1];
        assert!(line(&mut &data[..]).await.is_err());
        assert_eq!(line(&mut &b"hello\n"[..]).await.unwrap(), "hello\n");
        assert!(trusted_address("127.0.0.1:45130").is_ok());
        assert!(trusted_address("192.168.1.10:45130").is_ok());
        assert!(trusted_address("8.8.8.8:45130").is_err());
        assert!(trusted_address("0.0.0.0:45130").is_err());
    }

    #[tokio::test]
    async fn loopback_session_authenticates_and_delivers_state() {
        let reserve = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = reserve.local_addr().unwrap().to_string();
        drop(reserve);
        let initial = State {
            title: "Frieren".into(),
            episode: 3,
            duration: 1400.0,
            ..Default::default()
        };
        let (_send, receive) = watch::channel(initial);
        let code = crate::auth::secret();
        let host = host(&address, code.clone(), receive).await.unwrap();
        let (send, mut receive) = watch::channel(None);
        let guest = join(&address, &code, send).await.unwrap();
        tokio::time::timeout(Duration::from_secs(3), receive.changed())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(receive.borrow().as_ref().unwrap().episode, 3);
        assert_eq!(receive.borrow().as_ref().unwrap().title, "Frieren");
        guest.abort();
        host.abort();
        let _ = guest.await;
        let _ = host.await;
    }
}
