// am-helper: small native replacement for agent-monitor's helper modes (mcp-perm-server, run, __pty-leader, hook).

mod gosort;
mod hook;
mod host;
mod http;
mod json;
mod leader;
mod mcp;
mod pyjson;
mod screen;
mod sig;
mod util;

use std::io::Read;

/// Bump with rustHelperProtocol in helper_exec.go when a mode's contract changes.
const PROTOCOL: u32 = 1;

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let mode = args.get(1).map(|s| s.as_str()).unwrap_or("");
    let rest = if args.len() > 2 { &args[2..] } else { &[][..] };
    // Hooks keep the caller's PATH and locale, as the shell scripts do.
    if mode != "hook" {
        util::ensure_tool_env();
    }
    let code = match mode {
        "hook" => hook::run(rest),
        "mcp-perm-server" => mcp::run(),
        "run" => host::run_main(rest),
        "__pty-leader" => leader::run(rest),
        // Test hook: render stdin as the host's screen op would (cols rows lines).
        "__screen" => {
            let n = |i: usize, d: i64| rest.get(i).map(|s| util::atoi_lenient(s)).unwrap_or(d);
            let mut b = Vec::new();
            let _ = std::io::stdin().read_to_end(&mut b);
            print!("{}", screen::screen_text(&b, n(0, 80), n(1, 24), n(2, 0)));
            0
        }
        "__panic-test" => host::panic_selftest(),
        "version" | "-v" | "--version" => {
            // Second line: the protocol helper_exec.go checks before it uses this binary.
            println!("am-helper 0.3.0 (rust)\nprotocol {}", PROTOCOL);
            0
        }
        _ => {
            eprintln!("usage: am-helper mcp-perm-server | run <agent> [args...] | __pty-leader -- <cmd> [args...] | hook claude|codex");
            2
        }
    };
    std::process::exit(code);
}
