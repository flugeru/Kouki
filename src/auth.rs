use crate::api::Api;
use anyhow::{Context, Result, bail};
use base64::Engine;
use rand::RngCore;
use std::time::Duration;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
};
use url::Url;

pub fn secret() -> String {
    let mut bytes = [0u8; 32];
    rand::rng().fill_bytes(&mut bytes);
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(bytes)
}

// One bounded callback parser. No CORS, request bodies, or general HTTP server.
async fn request(stream: &mut TcpStream, expected_host: &str) -> Result<Url> {
    let mut bytes = Vec::new();
    loop {
        if bytes.len() >= 16 * 1024 {
            bail!("callback excede limite");
        }
        let byte = stream.read_u8().await?;
        bytes.push(byte);
        if bytes.ends_with(b"\r\n\r\n") {
            break;
        }
    }
    let text = std::str::from_utf8(&bytes)?;
    let mut lines = text.split("\r\n");
    let line = lines.next().context("callback vazio")?;
    let parts: Vec<_> = line.split_whitespace().collect();
    if parts.len() != 3
        || parts[0] != "GET"
        || !parts[1].starts_with('/')
        || parts[1].starts_with("//")
    {
        bail!("requisição inválida");
    }
    let host = lines
        .filter_map(|s| s.split_once(':'))
        .find(|(key, _)| key.eq_ignore_ascii_case("host"))
        .map(|(_, value)| value.trim());
    if host != Some(expected_host) {
        bail!("host inválido");
    }
    Ok(Url::parse(&format!("http://{expected_host}{}", parts[1]))?)
}

async fn respond(stream: &mut TcpStream, body: &str) -> Result<()> {
    let headers = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nCache-Control: no-store\r\nReferrer-Policy: no-referrer\r\nX-Content-Type-Options: nosniff\r\nContent-Security-Policy: default-src 'none'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'\r\nConnection: close\r\n\r\n",
        body.len()
    );
    stream.write_all(headers.as_bytes()).await?;
    stream.write_all(body.as_bytes()).await?;
    stream.shutdown().await?;
    Ok(())
}

pub async fn anilist(client_id: &str) -> Result<String> {
    let listener = TcpListener::bind("127.0.0.1:45124")
        .await
        .context("porta de login AniList 45124 ocupada")?;
    let state = secret();
    let mut url = Url::parse("https://anilist.co/api/v2/oauth/authorize")?;
    url.query_pairs_mut().extend_pairs([
        ("client_id", client_id),
        ("response_type", "token"),
        ("state", &state),
    ]);
    webbrowser::open(url.as_str())?;
    tokio::time::timeout(Duration::from_secs(180), async {
        loop {
            let (mut stream, _) = listener.accept().await?;
            let req = match tokio::time::timeout(Duration::from_secs(3), request(&mut stream, "127.0.0.1:45124")).await {
                Ok(Ok(url)) => url, _ => continue,
            };
            if req.path() == "/callback" {
                // Fragment never reaches HTTP. Same-origin navigation relays it;
                // state is checked on server, so injected callbacks cannot log in.
                let page = "<!doctype html><meta charset=utf-8><title>Kouki</title><p>Concluindo login…</p><script>const p=new URLSearchParams(location.hash.slice(1));history.replaceState(null,'','/callback');location.replace('/complete?'+p.toString())</script>";
                let _ = respond(&mut stream, page).await;
                continue;
            }
            if req.path() != "/complete" { continue; }
            let params: std::collections::HashMap<_, _> = req.query_pairs().into_owned().collect();
            if params.get("state") != Some(&state) {
                let _ = respond(&mut stream, "Login não reconhecido. Volte ao AniList.").await;
                continue;
            }
            if params.contains_key("error") { bail!("AniList recusou autorização"); }
            let token = params.get("access_token").filter(|s| !s.is_empty()).context("token ausente")?.clone();
            let _ = respond(&mut stream, "Login concluído. Pode fechar esta aba.").await;
            return Ok(token);
        }
    }).await.context("login AniList expirou")?
}

pub async fn mal(api: &Api, client_id: &str) -> Result<serde_json::Value> {
    if client_id.is_empty() {
        bail!("configure MAL client ID de aplicativo público/nativo");
    }
    let listener = TcpListener::bind("127.0.0.1:45125")
        .await
        .context("porta de login MAL 45125 ocupada")?;
    let state = secret();
    let verifier = secret();
    let mut url = Url::parse("https://myanimelist.net/v1/oauth2/authorize")?;
    url.query_pairs_mut().extend_pairs([
        ("client_id", client_id),
        ("response_type", "code"),
        ("state", &state),
        ("code_challenge", &verifier),
        ("code_challenge_method", "plain"),
        ("redirect_uri", "http://127.0.0.1:45125/callback"),
    ]);
    webbrowser::open(url.as_str())?;
    tokio::time::timeout(Duration::from_secs(180), async {
        loop {
            let (mut stream, _) = listener.accept().await?;
            let req = match tokio::time::timeout(
                Duration::from_secs(3),
                request(&mut stream, "127.0.0.1:45125"),
            )
            .await
            {
                Ok(Ok(req)) => req,
                _ => continue,
            };
            let params: std::collections::HashMap<_, _> = req.query_pairs().into_owned().collect();
            if req.path() != "/callback" || params.get("state") != Some(&state) {
                let _ = respond(&mut stream, "Login não reconhecido.").await;
                continue;
            }
            if params.contains_key("error") {
                bail!("MAL recusou autorização");
            }
            let code = params.get("code").context("código ausente")?;
            let token = api
                .mal_token(&[
                    ("client_id", client_id),
                    ("grant_type", "authorization_code"),
                    ("code", code),
                    ("code_verifier", &verifier),
                    ("redirect_uri", "http://127.0.0.1:45125/callback"),
                ])
                .await?;
            let _ = respond(&mut stream, "Login concluído. Pode fechar esta aba.").await;
            return Ok(token);
        }
    })
    .await
    .context("login MAL expirou")?
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn oauth_entropy_and_pkce_length() {
        let a = secret();
        assert_eq!(a.len(), 43);
        assert_ne!(a, secret());
        assert!(
            a.chars()
                .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
        );
    }

    #[tokio::test]
    async fn callback_rejects_foreign_host() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let client = tokio::spawn(async move {
            let mut stream = TcpStream::connect(address).await.unwrap();
            stream
                .write_all(b"GET /callback HTTP/1.1\r\nHost: attacker.example\r\n\r\n")
                .await
                .unwrap();
        });
        let (mut stream, _) = listener.accept().await.unwrap();
        assert!(request(&mut stream, &address.to_string()).await.is_err());
        client.await.unwrap();
    }
}
