use crate::{
    api::{Anime, Api, best_match},
    config::Settings,
    discord, media, mpv, simul,
};
use anyhow::{Context, Result, bail};
use serde_json::json;
use std::{
    sync::mpsc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::sync::{mpsc as channel, watch};

pub enum Command {
    Save(Box<Settings>),
    Launch(String),
    Stop,
    Player(&'static str),
    Search(String, bool),
    Select(Anime, bool),
    Login(bool),
    CancelLogin,
    Logout(bool),
    Host(String),
    Join(String, String),
    Leave,
    Quit,
}

pub enum Event {
    Status(String),
    Error(String),
    Playback(mpv::Playback),
    Anime(Option<Anime>, bool),
    Results(Vec<Anime>, bool),
    Accounts(crate::config::Accounts),
    Invite(String),
}

pub fn unix_now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

pub fn start(
    settings: Settings,
) -> (
    channel::Sender<Command>,
    mpsc::Receiver<Event>,
    std::thread::JoinHandle<()>,
) {
    let (tx, rx) = channel::channel(32);
    let (events, received) = mpsc::channel();
    let worker = std::thread::spawn(move || {
        let result = tokio::runtime::Runtime::new()
            .map_err(anyhow::Error::from)
            .and_then(|rt| rt.block_on(run(settings, rx, events.clone())));
        if let Err(err) = result {
            let _ = events.send(Event::Error(format!("{err:#}")));
        }
    });
    (tx, received, worker)
}

struct Resolved {
    file: String,
    ani: Option<Anime>,
    mal: Option<Anime>,
    error: Option<String>,
    filler: bool,
}
struct Tracking {
    file: String,
    parsed: media::Parsed,
    ani: Option<Anime>,
    mal: Option<Anime>,
    ani_done: bool,
    mal_done: bool,
    next_sync: u64,
}
impl Tracking {
    fn new() -> Self {
        Self {
            file: String::new(),
            parsed: Default::default(),
            ani: None,
            mal: None,
            ani_done: false,
            mal_done: false,
            next_sync: 0,
        }
    }
}

async fn run(
    mut settings: Settings,
    mut commands: channel::Receiver<Command>,
    events: mpsc::Sender<Event>,
) -> Result<()> {
    let api = Api::new()?;
    let mut ipc: Option<mpv::Ipc> = None;
    let mut child: Option<tokio::process::Child> = None;
    let mut tracking = Tracking::new();
    let mut discord = discord::Discord::new();
    let mut last_presence = 0;
    let mut last_manga = 0;
    let mut tick = tokio::time::interval(Duration::from_millis(700));
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let (resolved_tx, mut resolved_rx) = channel::channel::<Resolved>(4);
    let (search_tx, mut search_rx) = channel::channel::<(Result<Vec<Anime>>, bool)>(4);
    let (auth_tx, mut auth_rx) = channel::channel::<Result<(crate::config::Accounts, bool)>>(2);
    let mut resolver: Option<tokio::task::JoinHandle<()>> = None;
    let mut searcher: Option<tokio::task::JoinHandle<()>> = None;
    let mut login: Option<tokio::task::JoinHandle<()>> = None;
    let (host_tx, host_rx) = watch::channel(simul::State::default());
    let (join_tx, mut join_rx) = watch::channel(None::<simul::State>);
    let mut session: Option<tokio::task::JoinHandle<()>> = None;
    let mut joining = false;
    let mut remote_sync = (0i64, 0u32, 0u64);
    let _ = events.send(Event::Status("Aguardando MPV".into()));

    loop {
        tokio::select! {
            command = commands.recv() => {
                let Some(command) = command else { break };
                let result: Result<()> = match command {
                    Command::Quit => break,
                    Command::Save(new) => {
                        let mut new = *new;
                        new.accounts = settings.accounts.clone();
                        new.overrides = settings.overrides.clone();
                        new.mal_overrides = settings.mal_overrides.clone();
                        match new.save() {
                            Ok(()) => {
                                if new.ipc != settings.ipc { ipc = None; }
                                if settings.discord && !new.discord { let _ = discord.activity(serde_json::Value::Null).await; }
                                settings = new;
                                let _ = events.send(Event::Status("Preferências salvas".into()));
                                Ok(())
                            }
                            Err(err) => Err(err),
                        }
                    }
                    Command::Launch(file) => {
                        if child.as_mut().is_some_and(|c| matches!(c.try_wait(), Ok(None))) { Err(anyhow::anyhow!("MPV iniciado pelo Kouki já está aberto")) }
                        else {
                            match mpv::launch(&settings.mpv_path, &settings.ipc, &file).await {
                                Ok(process) => { child = Some(process); Ok(()) }, Err(err) => Err(err),
                            }
                        }
                    }
                    Command::Stop => {
                        if let Some(mut owned) = child.take() {
                            // Kill only owned child. External player's lifecycle remains user's.
                            if owned.try_wait()?.is_none() { owned.kill().await?; }
                        }
                        Ok(())
                    }
                    Command::Player(action) => {
                        if let Some(ipc) = ipc.as_mut() {
                            let cmd = match action {
                                "pause" => json!(["cycle", "pause"]), "next" => json!(["playlist-next"]),
                                "previous" => json!(["playlist-prev"]), _ => json!(["stop"]),
                            };
                            ipc.command(cmd).await.map(|_| ())
                        } else { Err(anyhow::anyhow!("MPV não conectado")) }
                    }
                    Command::Search(query, mal) => {
                        if let Some(task) = searcher.take() { task.abort(); }
                        let api = api.clone(); let client = settings.mal_client_id.clone(); let tx = search_tx.clone();
                        searcher = Some(tokio::spawn(async move {
                            let result = if mal { api.mal_search(&client, &query).await } else { api.anilist_search(&query).await };
                            let _ = tx.send((result, mal)).await;
                        }));
                        Ok(())
                    }
                    Command::Select(anime, mal) => {
                        if tracking.file.is_empty() { Err(anyhow::anyhow!("nenhum arquivo ativo")) }
                        else {
                            if let Some(task) = resolver.take() { task.abort(); }
                            if mal { settings.mal_overrides.insert(media::key(&tracking.parsed), anime.id); tracking.mal = Some(anime.clone()); tracking.mal_done = false; }
                            else { settings.overrides.insert(media::key(&tracking.parsed), anime.id); tracking.ani = Some(anime.clone()); tracking.ani_done = false; }
                            tracking.next_sync = 0;
                            let _ = events.send(Event::Anime(Some(anime), mal));
                            settings.save()
                        }
                    }
                    Command::Login(mal) => {
                        if let Some(task) = login.take() { task.abort(); }
                        let api = api.clone(); let cfg = settings.clone(); let tx = auth_tx.clone();
                        login = Some(tokio::spawn(async move {
                            let result = async {
                                let mut accounts = cfg.accounts.clone();
                                if mal {
                                    let tokens = crate::auth::mal(&api, &cfg.mal_client_id).await?;
                                    accounts.mal_access_token = tokens["access_token"].as_str().context("token MAL ausente")?.into();
                                    accounts.mal_refresh_token = tokens["refresh_token"].as_str().context("refresh token MAL ausente")?.into();
                                    accounts.mal_token_expires_at = unix_now() + tokens["expires_in"].as_u64().unwrap_or(0);
                                    accounts.mal_username = api.mal_viewer(&accounts.mal_access_token).await?;
                                } else {
                                    accounts.access_token = crate::auth::anilist(&cfg.anilist_client_id).await?;
                                    (accounts.user_id, accounts.username) = api.anilist_viewer(&accounts.access_token).await?;
                                }
                                Ok((accounts, mal))
                            }.await;
                            let _ = tx.send(result).await;
                        }));
                        let _ = events.send(Event::Status("Login aberto no navegador; prazo de 3 minutos".into()));
                        Ok(())
                    }
                    Command::CancelLogin => { if let Some(task) = login.take() { task.abort(); } while auth_rx.try_recv().is_ok() {} Ok(()) }
                    Command::Logout(mal) => {
                        if let Some(task) = login.take() { task.abort(); }
                        while auth_rx.try_recv().is_ok() {}
                        if mal { settings.accounts.mal_access_token.clear(); settings.accounts.mal_refresh_token.clear(); settings.accounts.mal_username.clear(); settings.accounts.mal_token_expires_at = 0; }
                        else { settings.accounts.access_token.clear(); settings.accounts.username.clear(); settings.accounts.user_id = 0; }
                        let _ = events.send(Event::Accounts(settings.accounts.clone()));
                        settings.save()
                    }
                    Command::Host(address) => {
                        if let Some(task) = session.take() { task.abort(); }
                        joining = false;
                        let code = crate::auth::secret();
                        match simul::host(&address, code.clone(), host_rx.clone()).await {
                            Ok(task) => { session = Some(task); let _ = events.send(Event::Invite(format!("Endereço: {address}\nCódigo: {code}"))); Ok(()) },
                            Err(err) => Err(err),
                        }
                    }
                    Command::Join(address, code) => {
                        if let Some(task) = session.take() { task.abort(); }
                        match simul::join(&address, &code, join_tx.clone()).await {
                            Ok(task) => { session = Some(task); joining = true; Ok(()) }, Err(err) => Err(err),
                        }
                    }
                    Command::Leave => { if let Some(task) = session.take() { task.abort(); } joining = false; let _ = join_tx.send(None); Ok(()) }
                };
                if let Err(err) = result { let _ = events.send(Event::Error(format!("{err:#}"))); }
            }
            Some(result) = resolved_rx.recv() => {
                if result.file == tracking.file {
                    if result.filler { let _ = events.send(Event::Error(format!("Aviso: episódio {} é filler (AnimeFillerList)", tracking.parsed.episode))); }
                    tracking.ani = result.ani; tracking.mal = result.mal;
                    let _ = events.send(Event::Anime(tracking.ani.clone(), false));
                    let _ = events.send(Event::Anime(tracking.mal.clone(), true));
                    if let Some(err) = result.error { let _ = events.send(Event::Error(err)); }
                }
            }
            Some((result, mal)) = search_rx.recv() => {
                match result { Ok(list) => { let _ = events.send(Event::Results(list, mal)); }, Err(err) => { let _ = events.send(Event::Error(err.to_string())); } }
            }
            Some(result) = auth_rx.recv() => {
                match result {
                    Ok((accounts, mal)) => {
                        if mal { settings.accounts.mal_access_token = accounts.mal_access_token; settings.accounts.mal_refresh_token = accounts.mal_refresh_token; settings.accounts.mal_token_expires_at = accounts.mal_token_expires_at; settings.accounts.mal_username = accounts.mal_username; tracking.mal_done = false; }
                        else { settings.accounts.access_token = accounts.access_token; settings.accounts.user_id = accounts.user_id; settings.accounts.username = accounts.username; tracking.ani_done = false; }
                        let _ = events.send(Event::Accounts(settings.accounts.clone()));
                        if let Err(err) = settings.save() { let _ = events.send(Event::Error(err.to_string())); }
                    }
                    Err(err) => { let _ = events.send(Event::Error(format!("login: {err:#}"))); }
                }
            }
            _ = join_rx.changed(), if joining => {
                let state = join_rx.borrow_and_update().clone();
                if let Some(state) = state {
                    let _ = events.send(Event::Status(format!("Simulwatching: {} · episódio {}", state.title, state.episode)));
                    if settings.discord { let _ = discord.activity(discord::watching(&state.title, state.episode, state.position, state.duration, state.paused, "")).await; }
                    // Never seek or change accounts merely because remote peer says so.
                    if settings.simul_sync && !state.paused && state.duration > 0.0 && state.position / state.duration >= 0.8 && state.anime_id > 0
                        && (remote_sync.0 != state.anime_id || remote_sync.1 != state.episode || unix_now() >= remote_sync.2) {
                        remote_sync = (state.anime_id, state.episode, unix_now() + 60);
                        let anime = Anime { id: state.anime_id, title: state.title, episodes: state.episodes, ..Default::default() };
                        match api.anilist_sync(&settings.accounts.access_token, settings.accounts.user_id, &anime, state.episode).await {
                            Ok(()) => remote_sync.2 = u64::MAX,
                            Err(err) => { let _ = events.send(Event::Error(err.to_string())); }
                        }
                    }
                } else { joining = false; let _ = events.send(Event::Status("Sessão remota encerrada".into())); }
            }
            _ = tick.tick() => {
                if joining { continue; }
                if ipc.is_none() { ipc = mpv::Ipc::connect(&settings.ipc).await.ok(); }
                let playback = if let Some(connection) = ipc.as_mut() { connection.playback().await.ok() } else { None };
                let Some(playback) = playback else {
                    ipc = None;
                    if !tracking.file.is_empty() {
                        tracking = Tracking::new();
                        let _ = events.send(Event::Playback(Default::default()));
                        let _ = events.send(Event::Anime(None, false));
                        let _ = events.send(Event::Anime(None, true));
                        if settings.discord { let _ = discord.activity(serde_json::Value::Null).await; }
                    }
                    if settings.discord && !settings.suwayomi.is_empty() && unix_now() > last_manga + 20 {
                        last_manga = unix_now();
                        if let Ok(Some((title, chapter))) = api.manga(&settings.suwayomi).await {
                            let _ = discord.activity(json!({"details":title,"state":chapter,"type":3})).await;
                        }
                    }
                    continue;
                };
                if playback.path != tracking.file {
                    if let Some(task) = resolver.take() { task.abort(); }
                    tracking = Tracking::new();
                    tracking.file = playback.path.clone();
                    tracking.parsed = media::parse(&playback.path);
                    if tracking.parsed.title.is_empty() { tracking.parsed = media::parse(&playback.title); }
                    let cfg = settings.clone(); let api = api.clone(); let file = tracking.file.clone(); let parsed = tracking.parsed.clone(); let tx = resolved_tx.clone();
                    let _ = events.send(Event::Anime(None, false)); let _ = events.send(Event::Anime(None, true));
                    resolver = Some(tokio::spawn(async move { let _ = tx.send(resolve(&api, &cfg, file, parsed).await).await; }));
                }
                let _ = events.send(Event::Playback(playback.clone()));
                let _ = events.send(Event::Status(if playback.paused { "Pausado" } else { "Reproduzindo" }.into()));
                let title = tracking.ani.as_ref().map_or(tracking.parsed.title.as_str(), |a| a.title.as_str());
                let _ = host_tx.send(simul::State { title: title.into(), episode: tracking.parsed.episode, anime_id: tracking.ani.as_ref().map_or(0, |a| a.id), episodes: tracking.ani.as_ref().map_or(0, |a| a.episodes), position: playback.position, duration: playback.duration, paused: playback.paused });
                if settings.discord && unix_now() >= last_presence + 10 {
                    last_presence = unix_now();
                    let cover = tracking.ani.as_ref().map_or("", |a| a.cover.as_str());
                    let _ = discord.activity(discord::watching(title, tracking.parsed.episode, playback.position, playback.duration, playback.paused, cover)).await;
                }
                if !playback.paused && playback.duration > 0.0 && playback.position / playback.duration >= 0.8 && tracking.parsed.episode > 0 && unix_now() >= tracking.next_sync {
                    tracking.next_sync = unix_now() + 60;
                    if settings.anilist_tracking && !tracking.ani_done && !settings.accounts.access_token.is_empty()
                        && let Some(anime) = &tracking.ani {
                            match api.anilist_sync(&settings.accounts.access_token, settings.accounts.user_id, anime, tracking.parsed.episode).await { Ok(()) => tracking.ani_done = true, Err(err) => { let _ = events.send(Event::Error(format!("AniList: {err}"))); } }
                    }
                    if settings.mal_tracking && !tracking.mal_done && !settings.accounts.mal_access_token.is_empty()
                        && let Some(anime) = &tracking.mal {
                            let result = async {
                                refresh_mal(&api, &mut settings).await?;
                                api.mal_sync(&settings.accounts.mal_access_token, anime, tracking.parsed.episode).await
                            }.await;
                            match result { Ok(()) => tracking.mal_done = true, Err(err) => { let _ = events.send(Event::Error(format!("MAL: {err}"))); } }
                    }
                }
            }
        }
    }
    for task in [resolver, searcher, login, session].into_iter().flatten() {
        task.abort();
    }
    let _ = discord.activity(serde_json::Value::Null).await;
    if let Some(mut process) = child
        && process.try_wait()?.is_none()
    {
        process.kill().await?;
    }
    Ok(())
}

async fn refresh_mal(api: &Api, settings: &mut Settings) -> Result<()> {
    if settings.accounts.mal_token_expires_at > unix_now() + 60 {
        return Ok(());
    }
    if settings.accounts.mal_refresh_token.is_empty() {
        bail!("MAL expirou; entre novamente");
    }
    let tokens = api
        .mal_refresh(
            &settings.mal_client_id,
            &settings.accounts.mal_refresh_token,
        )
        .await?;
    settings.accounts.mal_access_token = tokens["access_token"]
        .as_str()
        .context("MAL não retornou token")?
        .into();
    if let Some(token) = tokens["refresh_token"].as_str() {
        settings.accounts.mal_refresh_token = token.into();
    }
    settings.accounts.mal_token_expires_at = unix_now()
        + tokens["expires_in"]
            .as_u64()
            .context("MAL não retornou expiração")?;
    settings.save()
}

async fn resolve(api: &Api, cfg: &Settings, file: String, parsed: media::Parsed) -> Resolved {
    let key = media::key(&parsed);
    let ani = async {
        if let Some(id) = cfg.overrides.get(&key) {
            return api.anilist_by_id(*id).await.map(Some);
        }
        let results = api.anilist_search(&parsed.title).await?;
        Ok(best_match(&results, &parsed.title, parsed.season).cloned())
    };
    let mal = async {
        if cfg.mal_client_id.is_empty() {
            return Ok(None);
        }
        if let Some(id) = cfg.mal_overrides.get(&key) {
            return api.mal_by_id(&cfg.mal_client_id, *id).await.map(Some);
        }
        let results = api.mal_search(&cfg.mal_client_id, &parsed.title).await?;
        Ok(best_match(&results, &parsed.title, parsed.season).cloned())
    };
    let (ani, mal): (Result<Option<Anime>>, Result<Option<Anime>>) = tokio::join!(ani, mal);
    let errors: Vec<_> = [&ani, &mal]
        .iter()
        .filter_map(|r| r.as_ref().err().map(ToString::to_string))
        .collect();
    let filler = if cfg.filler && parsed.episode > 0 {
        let title = ani
            .as_ref()
            .ok()
            .and_then(|a| a.as_ref())
            .map_or(parsed.title.as_str(), |a| a.title.as_str());
        crate::filler::episodes(api, title)
            .await
            .is_ok_and(|episodes| episodes.contains(&parsed.episode))
    } else {
        false
    };
    Resolved {
        file,
        ani: ani.unwrap_or(None),
        mal: mal.unwrap_or(None),
        error: if errors.is_empty() {
            None
        } else {
            Some(errors.join("; "))
        },
        filler,
    }
}
