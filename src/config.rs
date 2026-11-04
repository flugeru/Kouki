use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::{collections::BTreeMap, fs, io::Write, path::PathBuf};

#[derive(Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct Settings {
    pub mpv_path: String,
    pub ipc: String,
    pub anilist_client_id: String,
    pub mal_client_id: String,
    // Tokens remain outside preferences, in a private file. Never Debug this struct.
    #[serde(skip)]
    pub accounts: Accounts,
    pub anilist_tracking: bool,
    pub mal_tracking: bool,
    pub discord: bool,
    pub filler: bool,
    pub suwayomi: String,
    pub simul_sync: bool,
    pub overrides: BTreeMap<String, i64>,
    pub mal_overrides: BTreeMap<String, i64>,
}

#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Accounts {
    pub access_token: String,
    pub username: String,
    pub user_id: i64,
    pub mal_access_token: String,
    pub mal_refresh_token: String,
    pub mal_token_expires_at: u64,
    pub mal_username: String,
}

impl Default for Settings {
    fn default() -> Self {
        Self {
            mpv_path: default_mpv(),
            ipc: default_ipc(),
            anilist_client_id: "54089".into(),
            mal_client_id: String::new(),
            accounts: Accounts::default(),
            anilist_tracking: true,
            mal_tracking: true,
            discord: true,
            filler: true,
            suwayomi: String::new(),
            simul_sync: false,
            overrides: BTreeMap::new(),
            mal_overrides: BTreeMap::new(),
        }
    }
}

pub fn dir() -> Result<PathBuf> {
    Ok(directories::BaseDirs::new()
        .context("diretório do usuário indisponível")?
        .config_dir()
        .join("Kouki"))
}

pub fn single_instance() -> Result<fs::File> {
    let root = dir()?;
    fs::create_dir_all(&root)?;
    let lock = fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(root.join("rust-instance.lock"))?;
    lock.try_lock()
        .context("outra instância Rust do Kouki já está aberta")?;
    Ok(lock)
}

fn default_mpv() -> String {
    if let Some(path) = std::env::var_os("KOUKI_MPV") {
        return path.to_string_lossy().into_owned();
    }
    #[cfg(windows)]
    if let Some(base) = std::env::var_os("LOCALAPPDATA") {
        for relative in ["Programs/mpv/mpv.exe", "Programs/Kouki/mpv/mpv.exe"] {
            let path = PathBuf::from(&base).join(relative);
            if path.is_file() {
                return path.to_string_lossy().into_owned();
            }
        }
    }
    if cfg!(windows) {
        "mpv.exe".into()
    } else {
        "mpv".into()
    }
}

pub fn default_ipc() -> String {
    if cfg!(windows) {
        return r"\\.\pipe\kouki-mpv".into();
    }
    let base = std::env::var_os("XDG_RUNTIME_DIR")
        .map(PathBuf::from)
        .or_else(|| dir().ok())
        .unwrap_or_else(|| PathBuf::from("."));
    base.join("kouki-mpv.sock").to_string_lossy().into_owned()
}

impl Settings {
    pub fn load() -> Result<Self> {
        let root = dir()?;
        let path = root.join("rust-settings.json");
        let mut settings = if path.exists() {
            serde_json::from_slice::<Self>(&fs::read(&path)?)
                .context("rust-settings.json inválido; arquivo preservado")?
        } else {
            Self::default()
        };
        let auth = root.join("rust-auth.json");
        if auth.exists() {
            settings.accounts = serde_json::from_slice(&fs::read(auth)?)
                .context("rust-auth.json inválido; arquivo preservado")?;
        } else if root.join("auth.json").exists() {
            // Import read-only: Go installation and its tokens stay untouched.
            settings.accounts = serde_json::from_slice(&fs::read(root.join("auth.json"))?)
                .context("auth.json legado inválido")?;
        }
        Ok(settings)
    }

    pub fn validate(&self) -> Result<()> {
        if self.mpv_path.trim().is_empty() {
            bail!("informe executável MPV");
        }
        if cfg!(windows) {
            if !self.ipc.starts_with(r"\\.\pipe\") {
                bail!("IPC precisa começar com \\\\.\\pipe\\");
            }
        } else if !std::path::Path::new(&self.ipc).is_absolute() {
            bail!("socket IPC precisa usar caminho absoluto");
        }
        if !self.suwayomi.is_empty() {
            let url = url::Url::parse(&self.suwayomi)?;
            if !matches!(url.scheme(), "http" | "https") || url.host_str().is_none() {
                bail!("URL Suwayomi inválida");
            }
        }
        Ok(())
    }

    pub fn save(&self) -> Result<()> {
        self.validate()?;
        let root = dir()?;
        fs::create_dir_all(&root)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&root, fs::Permissions::from_mode(0o700))?;
        }
        atomic_write(
            &root.join("rust-auth.json"),
            &serde_json::to_vec_pretty(&self.accounts)?,
        )?;
        atomic_write(
            &root.join("rust-settings.json"),
            &serde_json::to_vec_pretty(self)?,
        )
    }
}

pub fn atomic_write(path: &std::path::Path, bytes: &[u8]) -> Result<()> {
    let mut file =
        tempfile::NamedTempFile::new_in(path.parent().context("arquivo sem diretório")?)?;
    file.write_all(bytes)?;
    file.as_file().sync_all()?;
    file.persist(path).map_err(|err| err.error)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn private_atomic_replace_and_token_separation() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("test.json");
        atomic_write(&file, b"old").unwrap();
        atomic_write(&file, b"new").unwrap();
        assert_eq!(fs::read(file).unwrap(), b"new");
        let mut settings = Settings::default();
        settings.accounts.access_token = "SECRET".into();
        assert!(!serde_json::to_string(&settings).unwrap().contains("SECRET"));
    }

    #[test]
    fn native_file_lock_prevents_second_writer() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("lock");
        let first = fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(&path)
            .unwrap();
        first.try_lock().unwrap();
        let second = fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(&path)
            .unwrap();
        assert!(second.try_lock().is_err());
        drop(first);
        assert!(second.try_lock().is_ok());
    }
}
