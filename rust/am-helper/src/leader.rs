// Port of host_leader.go: the pty session leader that runs the agent in its own foreground group.

use std::io::Write;
use std::os::unix::process::CommandExt;
use std::process::Command;

pub fn run(args: &[String]) -> i32 {
    let mut args = args;
    if !args.is_empty() && args[0] == "--" {
        args = &args[1..];
    }
    if args.is_empty() {
        eprintln!("agent-monitor __pty-leader: agent required");
        return 2;
    }
    let pid_out = unsafe { libc::fcntl(3, libc::F_GETFD) } != -1;
    if pid_out {
        unsafe { libc::fcntl(3, libc::F_SETFD, libc::FD_CLOEXEC) };
    }
    // tcsetpgrp from the background needs SIGTTOU ignored; the agent inherits that.
    unsafe { libc::signal(libc::SIGTTOU, libc::SIG_IGN) };
    let sigs = [libc::SIGINT, libc::SIGTERM, libc::SIGHUP];
    let sig_fd = crate::sig::notify(&sigs);

    let mut cmd = Command::new(&args[0]);
    cmd.args(&args[1..]);
    unsafe {
        cmd.pre_exec(|| {
            if libc::setpgid(0, 0) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            let pg = libc::getpid();
            if libc::ioctl(0, libc::TIOCSPGRP, &pg) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    let child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            let _ = write!(std::io::stderr(), "agent-monitor: fork/exec {}: {}\r\n", args[0], crate::util::io_err_text(&e));
            return 126;
        }
    };
    let pid = child.id() as i32;
    if pid_out {
        let s = format!("{}\n", pid);
        unsafe {
            libc::write(3, s.as_ptr() as *const libc::c_void, s.len());
            libc::close(3);
        }
    }
    crate::host::spawn_small(move || {
        while let Some(s) = crate::sig::next(sig_fd) {
            unsafe { libc::kill(-pid, s) };
        }
    });
    loop {
        let st = match crate::sig::wait_untraced(pid) {
            Ok(st) => st,
            Err(_) => return 1,
        };
        if crate::sig::stopped(st) {
            // SIGSTOP is never dropped; the host sees this stop and sends SIGCONT when the user resumes.
            unsafe {
                libc::kill(libc::getpid(), libc::SIGSTOP);
                libc::ioctl(0, libc::TIOCSPGRP, &pid);
                libc::kill(-pid, libc::SIGCONT);
            }
            continue;
        }
        if let Some(c) = crate::sig::code_of(st) {
            return c;
        }
    }
}
