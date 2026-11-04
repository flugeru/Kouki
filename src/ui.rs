use crate::{
    api::Anime,
    config::Settings,
    mpv::Playback,
    runtime::{Command, Event},
};
use eframe::egui;
use std::{collections::VecDeque, sync::mpsc, time::Duration};

pub struct App {
    settings: Settings,
    commands: tokio::sync::mpsc::Sender<Command>,
    events: mpsc::Receiver<Event>,
    playback: Playback,
    ani: Option<Anime>,
    mal: Option<Anime>,
    status: String,
    errors: VecDeque<String>,
    file: String,
    query: String,
    search_mal: bool,
    results: Vec<Anime>,
    address: String,
    code: String,
    invite: String,
    tab: usize,
}

impl App {
    pub fn new(
        settings: Settings,
        commands: tokio::sync::mpsc::Sender<Command>,
        events: mpsc::Receiver<Event>,
    ) -> Self {
        Self {
            settings,
            commands,
            events,
            playback: Default::default(),
            ani: None,
            mal: None,
            status: "Iniciando".into(),
            errors: VecDeque::new(),
            file: String::new(),
            query: String::new(),
            search_mal: false,
            results: vec![],
            address: "127.0.0.1:45130".into(),
            code: String::new(),
            invite: String::new(),
            tab: 0,
        }
    }

    fn send(&mut self, cmd: Command) {
        if self.commands.try_send(cmd).is_err() {
            self.error("Operação pendente; aguarde e tente novamente".into());
        }
    }
    fn error(&mut self, text: String) {
        self.errors.push_back(text);
        if self.errors.len() > 20 {
            self.errors.pop_front();
        }
    }

    fn playback_ui(&mut self, ui: &mut egui::Ui) {
        ui.heading(
            self.ani
                .as_ref()
                .map_or("Nada identificado", |a| a.title.as_str()),
        );
        ui.label(&self.playback.title);
        ui.label(egui::RichText::new(&self.playback.path).small().weak());
        let progress = if self.playback.duration > 0.0 {
            (self.playback.position / self.playback.duration).clamp(0.0, 1.0) as f32
        } else {
            0.0
        };
        ui.add(egui::ProgressBar::new(progress).text(format!(
            "{} / {}",
            clock(self.playback.position),
            clock(self.playback.duration)
        )));
        ui.horizontal(|ui| {
            if ui.button("Anterior").clicked() {
                self.send(Command::Player("previous"));
            }
            if ui
                .button(if self.playback.paused {
                    "Retomar"
                } else {
                    "Pausar"
                })
                .clicked()
            {
                self.send(Command::Player("pause"));
            }
            if ui.button("Próximo").clicked() {
                self.send(Command::Player("next"));
            }
            if ui.button("Parar mídia").clicked() {
                self.send(Command::Player("stop"));
            }
        });
        ui.separator();
        ui.label("Arquivo ou URL (vazio abre player em espera)");
        ui.text_edit_singleline(&mut self.file);
        ui.horizontal(|ui| {
            if ui.button("Iniciar MPV").clicked() {
                self.send(Command::Launch(self.file.clone()));
            }
            if ui.button("Encerrar MPV iniciado pelo Kouki").clicked() {
                self.send(Command::Stop);
            }
        });
        ui.separator();
        ui.heading("Corrigir identificação");
        ui.horizontal(|ui| {
            ui.text_edit_singleline(&mut self.query);
            ui.checkbox(&mut self.search_mal, "MyAnimeList");
            if ui.button("Pesquisar").clicked() && !self.query.trim().is_empty() {
                self.send(Command::Search(self.query.clone(), self.search_mal));
            }
        });
        let mut selected = None;
        for anime in &self.results {
            ui.horizontal(|ui| {
                ui.label(format!(
                    "{} · {} · {} episódios",
                    anime.title, anime.year, anime.episodes
                ));
                if ui
                    .add_enabled(
                        !self.playback.path.is_empty(),
                        egui::Button::new("Selecionar"),
                    )
                    .clicked()
                {
                    selected = Some(anime.clone());
                }
            });
        }
        if let Some(anime) = selected {
            self.send(Command::Select(anime, self.search_mal));
            self.results.clear();
        }
    }

    fn accounts_ui(&mut self, ui: &mut egui::Ui) {
        for mal in [false, true] {
            ui.heading(if mal { "MyAnimeList" } else { "AniList" });
            let username = if mal {
                &self.settings.accounts.mal_username
            } else {
                &self.settings.accounts.username
            };
            ui.label(if username.is_empty() {
                "Nenhuma conta conectada"
            } else {
                username
            });
            ui.horizontal(|ui| {
                if ui.button("Entrar pelo navegador").clicked() {
                    self.send(Command::Login(mal));
                }
                if ui.button("Sair da conta").clicked() {
                    self.send(Command::Logout(mal));
                }
            });
            ui.checkbox(
                if mal {
                    &mut self.settings.mal_tracking
                } else {
                    &mut self.settings.anilist_tracking
                },
                "Sincronizar ao atingir 80% do episódio",
            );
            let anime = if mal { &self.mal } else { &self.ani };
            if let Some(anime) = anime {
                ui.label(format!("Identificado: {} (ID {})", anime.title, anime.id));
            }
            ui.separator();
        }
        if ui.button("Cancelar login em andamento").clicked() {
            self.send(Command::CancelLogin);
        }
        ui.label("AniList: callback registrado em http://127.0.0.1:45124/callback.");
        ui.label("MAL: use client ID público/nativo e callback http://127.0.0.1:45125/callback.");
        ui.label("Credenciais de aplicativos confidenciais não são embutidas no executável.");
        if ui.button("Salvar opções de acompanhamento").clicked() {
            self.send(Command::Save(Box::new(self.settings.clone())));
        }
    }

    fn settings_ui(&mut self, ui: &mut egui::Ui) {
        ui.heading("Configurações");
        egui::Grid::new("settings").num_columns(2).show(ui, |ui| {
            ui.label("Executável MPV");
            ui.text_edit_singleline(&mut self.settings.mpv_path);
            ui.end_row();
            ui.label("Canal IPC");
            ui.text_edit_singleline(&mut self.settings.ipc);
            ui.end_row();
            ui.label("AniList client ID");
            ui.text_edit_singleline(&mut self.settings.anilist_client_id);
            ui.end_row();
            ui.label("MAL client ID");
            ui.text_edit_singleline(&mut self.settings.mal_client_id);
            ui.end_row();
            ui.label("Servidor Suwayomi");
            ui.text_edit_singleline(&mut self.settings.suwayomi);
            ui.end_row();
        });
        ui.checkbox(&mut self.settings.discord, "Discord Rich Presence");
        ui.checkbox(
            &mut self.settings.filler,
            "Avisar episódios filler (correspondência exata de título)",
        );
        ui.label("MPV aberto pelo Kouki recebe canal IPC automaticamente. Para player externo, configure mpv.conf:");
        ui.code(format!("input-ipc-server={}", self.settings.ipc));
        ui.label("Não configure IPC em diretório público nem exponha controle MPV pela rede.");
        if ui.button("Salvar configurações").clicked() {
            match self.settings.validate() {
                Ok(()) => self.send(Command::Save(Box::new(self.settings.clone()))),
                Err(err) => self.error(err.to_string()),
            }
        }
        if let Ok(dir) = crate::config::dir() {
            ui.label(format!("Dados: {}", dir.display()));
        }
        ui.separator();
        ui.label("Android adiado: integração is.xyz.mpv depende de teste de sandbox e IPC.");
    }

    fn simul_ui(&mut self, ui: &mut egui::Ui) {
        ui.heading("Simulwatching");
        ui.label(
            "Compartilha presença e progresso; não transmite vídeo nem abre arquivos remotos.",
        );
        ui.colored_label(egui::Color32::YELLOW, "Transporte TCP sem criptografia. Use apenas localhost, LAN confiável ou VPN. Não abra porta no roteador.");
        ui.label("Endereço IP:porta para hospedar ou entrar");
        ui.text_edit_singleline(&mut self.address);
        ui.label("Código recebido do anfitrião");
        ui.add(egui::TextEdit::singleline(&mut self.code).password(true));
        ui.horizontal(|ui| {
            if ui.button("Hospedar e gerar convite").clicked() {
                self.send(Command::Host(self.address.clone()));
            }
            if ui.button("Entrar").clicked() {
                self.send(Command::Join(self.address.clone(), self.code.clone()));
            }
            if ui.button("Encerrar sessão").clicked() {
                self.send(Command::Leave);
            }
        });
        if !self.invite.is_empty() {
            ui.label(&self.invite);
            if ui.button("Copiar convite").clicked() {
                ui.ctx().copy_text(self.invite.clone());
            }
        }
        ui.checkbox(
            &mut self.settings.simul_sync,
            "Autorizar anfitrião a atualizar meu progresso AniList",
        );
        if ui.button("Salvar autorização").clicked() {
            self.send(Command::Save(Box::new(self.settings.clone())));
        }
    }
}

impl eframe::App for App {
    fn update(&mut self, ctx: &egui::Context, _frame: &mut eframe::Frame) {
        while let Ok(event) = self.events.try_recv() {
            match event {
                Event::Status(status) => self.status = status,
                Event::Error(err) => self.error(err),
                Event::Playback(playback) => self.playback = playback,
                Event::Anime(anime, mal) => {
                    if mal {
                        self.mal = anime
                    } else {
                        self.ani = anime
                    }
                }
                Event::Results(list, mal) => {
                    self.results = list;
                    self.search_mal = mal;
                }
                Event::Accounts(accounts) => self.settings.accounts = accounts,
                Event::Invite(invite) => self.invite = invite,
            }
        }
        ctx.set_visuals(egui::Visuals::dark());
        egui::TopBottomPanel::top("top").show(ctx, |ui| {
            ui.horizontal(|ui| {
                ui.heading("Kouki");
                ui.label(env!("CARGO_PKG_VERSION"));
                ui.separator();
                ui.label(&self.status);
            });
            ui.horizontal(|ui| {
                for (index, label) in ["Reprodução", "Contas", "Simulwatching", "Configurações"]
                    .iter()
                    .enumerate()
                {
                    ui.selectable_value(&mut self.tab, index, *label);
                }
            });
        });
        egui::TopBottomPanel::bottom("errors").show(ctx, |ui| {
            if let Some(err) = self.errors.back() {
                ui.colored_label(egui::Color32::LIGHT_RED, err);
                if ui.button("Dispensar aviso").clicked() {
                    self.errors.pop_back();
                }
            }
            ui.label("MPV externo · Windows / Linux · núcleo Rust compartilhado");
        });
        egui::CentralPanel::default().show(ctx, |ui| {
            egui::ScrollArea::vertical().show(ui, |ui| match self.tab {
                0 => self.playback_ui(ui),
                1 => self.accounts_ui(ui),
                2 => self.simul_ui(ui),
                _ => self.settings_ui(ui),
            });
        });
        ctx.request_repaint_after(Duration::from_millis(200));
    }
    fn on_exit(&mut self, _gl: Option<&eframe::glow::Context>) {
        let _ = self.commands.try_send(Command::Quit);
    }
}

fn clock(seconds: f64) -> String {
    let seconds = if seconds.is_finite() {
        seconds.max(0.0) as u64
    } else {
        0
    };
    format!(
        "{:02}:{:02}:{:02}",
        seconds / 3600,
        seconds / 60 % 60,
        seconds % 60
    )
}
