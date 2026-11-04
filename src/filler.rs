use anyhow::{Result, bail};
use regex::Regex;
use std::{collections::BTreeSet, sync::LazyLock};

static SHOW: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r#"<a\s+href="(/shows/[a-z0-9-]+)"[^>]*>([^<]+)</a>"#).unwrap());
static ROW: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?is)<tr[^>]*class="[^"]*\bfiller\b[^"]*"[^>]*>.*?<td[^>]*class="[^"]*\bNumber\b[^"]*"[^>]*>\s*(\d+)"#).unwrap()
});

async fn page(api: &crate::api::Api, url: &str) -> Result<String> {
    let mut response = api.http.get(url).send().await?.error_for_status()?;
    if response.url().host_str() != Some("www.animefillerlist.com") {
        bail!("redirecionamento filler inesperado");
    }
    let mut bytes = Vec::new();
    while let Some(chunk) = response.chunk().await? {
        if bytes.len() + chunk.len() > 4 * 1024 * 1024 {
            bail!("página filler excede limite");
        }
        bytes.extend_from_slice(&chunk);
    }
    Ok(String::from_utf8(bytes)?)
}

pub async fn episodes(api: &crate::api::Api, title: &str) -> Result<BTreeSet<u32>> {
    let shows = page(api, "https://www.animefillerlist.com/shows").await?;
    // ponytail: exact normalized title only; add explicit slug override for aliases.
    let normalize = |s: &str| {
        s.to_lowercase()
            .chars()
            .filter(|c| c.is_alphanumeric())
            .collect::<String>()
    };
    let wanted = normalize(title);
    let slug = SHOW
        .captures_iter(&shows)
        .find(|cap| normalize(&cap[2]) == wanted)
        .map(|cap| cap[1].to_owned());
    let Some(slug) = slug else {
        return Ok(BTreeSet::new());
    };
    let html = page(api, &format!("https://www.animefillerlist.com{slug}")).await?;
    Ok(parse(&html))
}

fn parse(html: &str) -> BTreeSet<u32> {
    ROW.captures_iter(html)
        .filter_map(|cap| cap[1].parse().ok())
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn filler_not_mixed_canon() {
        let html = r#"<tr class="filler"><td class="Number">12</td></tr><tr class="manga_canon"><td class="Number">13</td></tr>"#;
        assert_eq!(parse(html), BTreeSet::from([12]));
    }
}
