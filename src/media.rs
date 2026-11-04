use regex::Regex;
use std::sync::LazyLock;

static BRACKETS: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"\[[^\]]*\]|\([^)]*(?:1080|720|2160|480|HEVC|x26[45])[^)]*\)").unwrap()
});
static EPISODE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)(?:S(\d{1,2})E|\s-\s|\bEP?\s*)(\d{1,4})(?:v\d)?(?:\D|$)").unwrap()
});
static TRAILING: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"\s+(\d{1,3})(?:v\d)?\s*$").unwrap());

#[derive(Clone, Default, Debug, PartialEq)]
pub struct Parsed {
    pub title: String,
    pub episode: u32,
    pub season: u32,
}

pub fn parse(path: &str) -> Parsed {
    let path = url::Url::parse(path)
        .ok()
        .filter(|u| matches!(u.scheme(), "http" | "https" | "file"))
        .map(|u| {
            percent_encoding::percent_decode_str(u.path())
                .decode_utf8_lossy()
                .into_owned()
        })
        .unwrap_or_else(|| path.to_owned());
    let file = path.rsplit(['/', '\\']).next().unwrap_or(&path);
    let stem = file
        .rsplit_once('.')
        .filter(|(_, ext)| {
            matches!(
                ext.to_ascii_lowercase().as_str(),
                "mkv" | "mp4" | "avi" | "webm" | "m4v" | "ts"
            )
        })
        .map_or(file, |(s, _)| s);
    let cleaned = BRACKETS.replace_all(stem, " ").replace('_', " ");
    let captures = EPISODE
        .captures(&cleaned)
        .or_else(|| TRAILING.captures(&cleaned));
    if let Some(cap) = captures {
        let full = cap.get(0).unwrap();
        let (season, episode) = if cap.len() > 2 {
            (
                cap.get(1)
                    .and_then(|v| v.as_str().parse().ok())
                    .unwrap_or(0),
                cap[2].parse().unwrap_or(0),
            )
        } else {
            (0, cap[1].parse().unwrap_or(0))
        };
        let title = cleaned[..full.start()]
            .trim_matches([' ', '-', '.'])
            .replace('.', " ");
        return Parsed {
            title: normalize_spaces(&title),
            episode,
            season,
        };
    }
    Parsed {
        title: normalize_spaces(&cleaned.replace('.', " ")),
        ..Default::default()
    }
}

pub fn key(parsed: &Parsed) -> String {
    format!("{}|s{}", parsed.title.to_lowercase(), parsed.season)
}
fn normalize_spaces(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn filenames() {
        assert_eq!(
            parse("[SubsPlease] Frieren - 03 (1080p) [ABC123].mkv").episode,
            3
        );
        assert_eq!(
            parse("C:\\Anime\\Frieren.S02E03.mkv"),
            Parsed {
                title: "Frieren".into(),
                episode: 3,
                season: 2
            }
        );
        assert_eq!(parse("Show 12.mkv").episode, 12);
        assert_eq!(parse("Movie 2024.mkv").episode, 0);
        assert_eq!(parse("https://example.org/Show%20-%2002.mkv").episode, 2);
    }
}
