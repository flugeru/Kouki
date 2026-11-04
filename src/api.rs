use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::time::Duration;

#[derive(Clone)]
pub struct Api {
    pub http: reqwest::Client,
}

#[derive(Clone, Default, Debug, Serialize, Deserialize)]
pub struct Anime {
    pub id: i64,
    pub title: String,
    pub episodes: u32,
    pub year: u32,
    pub cover: String,
}

impl Api {
    pub fn new() -> Result<Self> {
        Ok(Self {
            http: reqwest::Client::builder()
                .timeout(Duration::from_secs(12))
                .user_agent(concat!("Kouki/", env!("CARGO_PKG_VERSION")))
                .build()?,
        })
    }

    pub async fn gql(&self, token: &str, query: &str, variables: Value) -> Result<Value> {
        let mut req = self
            .http
            .post("https://graphql.anilist.co")
            .json(&json!({"query": query, "variables": variables}));
        if !token.is_empty() {
            req = req.bearer_auth(token);
        }
        let response: Value = req.send().await?.error_for_status()?.json().await?;
        if response.get("errors").is_some() {
            bail!("AniList: {}", response["errors"]);
        }
        Ok(response["data"].clone())
    }

    pub async fn anilist_search(&self, query: &str) -> Result<Vec<Anime>> {
        let data = self.gql("", "query($q:String){Page(perPage:12){media(search:$q,type:ANIME){id title{romaji english native} episodes startDate{year} coverImage{large}}}}", json!({"q": query})).await?;
        Ok(data["Page"]["media"]
            .as_array()
            .context("resposta AniList inválida")?
            .iter()
            .map(anilist_anime)
            .collect())
    }

    pub async fn anilist_by_id(&self, id: i64) -> Result<Anime> {
        let data = self.gql("", "query($id:Int){Media(id:$id,type:ANIME){id title{romaji english native} episodes startDate{year} coverImage{large}}}", json!({"id": id})).await?;
        Ok(anilist_anime(&data["Media"]))
    }

    pub async fn anilist_viewer(&self, token: &str) -> Result<(i64, String)> {
        let data = self.gql(token, "query{Viewer{id name}}", json!({})).await?;
        Ok((
            data["Viewer"]["id"]
                .as_i64()
                .context("usuário AniList ausente")?,
            data["Viewer"]["name"]
                .as_str()
                .context("nome AniList ausente")?
                .into(),
        ))
    }

    pub async fn anilist_sync(
        &self,
        token: &str,
        user: i64,
        anime: &Anime,
        episode: u32,
    ) -> Result<()> {
        if token.is_empty() || user <= 0 || anime.id <= 0 || episode == 0 {
            bail!("conta ou episódio AniList inválido");
        }
        let old = self
            .gql(
                token,
                "query($u:Int,$m:Int){MediaList(userId:$u,mediaId:$m){progress status}}",
                json!({"u": user, "m": anime.id}),
            )
            .await;
        let old = match old {
            Ok(data) => data["MediaList"].clone(),
            // Missing list entry is a GraphQL Not Found, not a network failure.
            Err(err) if err.to_string().contains("Not Found") => Value::Null,
            Err(err) => return Err(err),
        };
        if old["progress"].as_u64().unwrap_or(0) >= u64::from(episode)
            && old["status"] != "REPEATING"
        {
            return Ok(());
        }
        let progress = if anime.episodes > 0 {
            episode.min(anime.episodes)
        } else {
            episode
        };
        let status = if anime.episodes > 0 && progress >= anime.episodes {
            "COMPLETED"
        } else if old["status"] == "REPEATING" {
            "REPEATING"
        } else {
            "CURRENT"
        };
        self.gql(token, "mutation($m:Int,$p:Int,$s:MediaListStatus){SaveMediaListEntry(mediaId:$m,progress:$p,status:$s){id}}", json!({"m": anime.id, "p": progress, "s": status})).await?;
        Ok(())
    }

    pub async fn mal_search(&self, client: &str, query: &str) -> Result<Vec<Anime>> {
        if client.is_empty() {
            bail!("configure MAL client ID");
        }
        let data: Value = self
            .http
            .get("https://api.myanimelist.net/v2/anime")
            .header("X-MAL-CLIENT-ID", client)
            .query(&[
                ("q", query),
                ("limit", "12"),
                ("fields", "num_episodes,start_date"),
            ])
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        Ok(data["data"]
            .as_array()
            .context("resposta MAL inválida")?
            .iter()
            .map(|r| mal_anime(&r["node"]))
            .collect())
    }

    pub async fn mal_by_id(&self, client: &str, id: i64) -> Result<Anime> {
        let data: Value = self
            .http
            .get(format!("https://api.myanimelist.net/v2/anime/{id}"))
            .header("X-MAL-CLIENT-ID", client)
            .query(&[("fields", "num_episodes,start_date")])
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        Ok(mal_anime(&data))
    }

    pub async fn mal_viewer(&self, token: &str) -> Result<String> {
        let data: Value = self
            .http
            .get("https://api.myanimelist.net/v2/users/@me")
            .bearer_auth(token)
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        Ok(data["name"].as_str().context("nome MAL ausente")?.into())
    }

    pub async fn mal_refresh(&self, client: &str, refresh: &str) -> Result<Value> {
        self.mal_token(&[
            ("client_id", client),
            ("grant_type", "refresh_token"),
            ("refresh_token", refresh),
        ])
        .await
    }

    pub async fn mal_token(&self, fields: &[(&str, &str)]) -> Result<Value> {
        let response = self
            .http
            .post("https://myanimelist.net/v1/oauth2/token")
            .form(fields)
            .send()
            .await?;
        // Do not include response body: OAuth errors can contain credentials.
        Ok(response.error_for_status()?.json().await?)
    }

    pub async fn mal_sync(&self, token: &str, anime: &Anime, episode: u32) -> Result<()> {
        if token.is_empty() || anime.id <= 0 || episode == 0 {
            bail!("conta ou episódio MAL inválido");
        }
        let current: Value = self
            .http
            .get(format!("https://api.myanimelist.net/v2/anime/{}", anime.id))
            .bearer_auth(token)
            .query(&[("fields", "my_list_status")])
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        let old = &current["my_list_status"];
        if old["num_episodes_watched"].as_u64().unwrap_or(0) >= u64::from(episode)
            && old["is_rewatching"] != true
        {
            return Ok(());
        }
        let ep = if anime.episodes > 0 {
            episode.min(anime.episodes)
        } else {
            episode
        };
        let status = if anime.episodes > 0 && ep >= anime.episodes {
            "completed"
        } else {
            "watching"
        };
        self.http
            .put(format!(
                "https://api.myanimelist.net/v2/anime/{}/my_list_status",
                anime.id
            ))
            .bearer_auth(token)
            .form(&[
                ("num_watched_episodes", ep.to_string()),
                ("status", status.into()),
            ])
            .send()
            .await?
            .error_for_status()?;
        Ok(())
    }

    pub async fn manga(&self, base: &str) -> Result<Option<(String, String)>> {
        let url = format!("{}/api/graphql", base.trim_end_matches('/'));
        let data: Value = self.http.post(url).json(&json!({"query":"query { chapters(condition: {isRead: true}, order: [{by: LAST_READ_AT, byType: DESC}], first: 1) { nodes { name chapterNumber lastReadAt manga { title } } } }"})).send().await?.error_for_status()?.json().await?;
        let chapter = &data["data"]["chapters"]["nodes"][0];
        Ok(chapter["manga"]["title"].as_str().map(|title| {
            (
                title.to_owned(),
                chapter["name"].as_str().unwrap_or("Lendo mangá").into(),
            )
        }))
    }
}

fn anilist_anime(v: &Value) -> Anime {
    Anime {
        id: v["id"].as_i64().unwrap_or(0),
        title: v["title"]["romaji"]
            .as_str()
            .or(v["title"]["english"].as_str())
            .or(v["title"]["native"].as_str())
            .unwrap_or("Sem título")
            .into(),
        episodes: v["episodes"].as_u64().unwrap_or(0) as u32,
        year: v["startDate"]["year"].as_u64().unwrap_or(0) as u32,
        cover: v["coverImage"]["large"].as_str().unwrap_or_default().into(),
    }
}

fn mal_anime(v: &Value) -> Anime {
    Anime {
        id: v["id"].as_i64().unwrap_or(0),
        title: v["title"].as_str().unwrap_or("Sem título").into(),
        episodes: v["num_episodes"].as_u64().unwrap_or(0) as u32,
        year: v["start_date"]
            .as_str()
            .and_then(|s| s.get(..4))
            .and_then(|s| s.parse().ok())
            .unwrap_or(0),
        cover: v["main_picture"]["large"]
            .as_str()
            .or(v["main_picture"]["medium"].as_str())
            .unwrap_or_default()
            .into(),
    }
}

pub fn best_match<'a>(results: &'a [Anime], title: &str, season: u32) -> Option<&'a Anime> {
    fn words(s: &str) -> std::collections::BTreeSet<String> {
        s.to_lowercase()
            .split(|c: char| !c.is_alphanumeric())
            .filter(|s| !s.is_empty())
            .map(str::to_owned)
            .collect()
    }
    let wanted = words(title);
    results
        .iter()
        .max_by_key(|a| {
            let candidate = words(&a.title);
            let overlap = wanted.intersection(&candidate).count() as i32;
            let extra = candidate.difference(&wanted).count() as i32;
            overlap * 10 - extra
                + if a.title.to_lowercase() == title.to_lowercase() {
                    100
                } else {
                    0
                }
                + if season > 1 && a.title.contains(&season.to_string()) {
                    10
                } else {
                    0
                }
        })
        .filter(|a| !wanted.is_disjoint(&words(&a.title)))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn matching_prefers_exact_and_rejects_unrelated() {
        let results = vec![
            Anime {
                title: "Naruto Shippuden".into(),
                ..Default::default()
            },
            Anime {
                title: "Naruto".into(),
                ..Default::default()
            },
        ];
        assert_eq!(best_match(&results, "Naruto", 0).unwrap().title, "Naruto");
        assert!(best_match(&results, "Frieren", 0).is_none());
    }
}
