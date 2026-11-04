#![cfg_attr(all(windows, not(debug_assertions)), windows_subsystem = "windows")]

mod api;
mod auth;
mod config;
mod discord;
mod filler;
mod media;
mod mpv;
mod runtime;
mod simul;
mod ui;

fn main() -> anyhow::Result<()> {
    if std::env::args().any(|arg| arg == "--version") {
        println!(
            "Kouki {} ({}/{})",
            env!("CARGO_PKG_VERSION"),
            std::env::consts::OS,
            std::env::consts::ARCH
        );
        return Ok(());
    }
    let _instance = config::single_instance()?;
    let settings = config::Settings::load()?;
    let (commands, events, worker) = runtime::start(settings.clone());
    let options = eframe::NativeOptions {
        viewport: eframe::egui::ViewportBuilder::default()
            .with_title("Kouki — contas, reprodução e progresso")
            .with_inner_size([920.0, 740.0])
            .with_min_inner_size([600.0, 480.0]),
        ..Default::default()
    };
    let result = eframe::run_native(
        "Kouki",
        options,
        Box::new(move |_| Ok(Box::new(ui::App::new(settings, commands, events)))),
    )
    .map_err(|err| anyhow::anyhow!("interface: {err}"));
    worker
        .join()
        .map_err(|_| anyhow::anyhow!("worker encerrado inesperadamente"))?;
    result
}
