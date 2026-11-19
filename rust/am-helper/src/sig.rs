// Self-pipe signal delivery: handlers write the signal number, a thread reads it (Go's signal.Notify).

use std::sync::atomic::{AtomicI32, Ordering};

static PIPE_W: AtomicI32 = AtomicI32::new(-1);

extern "C" fn on_signal(sig: libc::c_int) {
    let fd = PIPE_W.load(Ordering::Relaxed);
    if fd >= 0 {
        let b = sig as u8;
        let saved = unsafe { *errno_ptr() };
        unsafe {
            libc::write(fd, &b as *const u8 as *const libc::c_void, 1);
            *errno_ptr() = saved;
        }
    }
}

#[cfg(target_os = "macos")]
unsafe fn errno_ptr() -> *mut libc::c_int {
    libc::__error()
}

#[cfg(target_os = "linux")]
unsafe fn errno_ptr() -> *mut libc::c_int {
    libc::__errno_location()
}

/// Installs handlers for sigs and returns the read end of the pipe they write to.
pub fn notify(sigs: &[libc::c_int]) -> i32 {
    let mut fds = [0i32; 2];
    unsafe {
        libc::pipe(fds.as_mut_ptr());
        for fd in fds {
            libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC);
        }
        libc::fcntl(fds[1], libc::F_SETFL, libc::O_NONBLOCK);
    }
    PIPE_W.store(fds[1], Ordering::Relaxed);
    for &s in sigs {
        unsafe {
            let mut sa: libc::sigaction = std::mem::zeroed();
            sa.sa_sigaction = on_signal as extern "C" fn(libc::c_int) as usize;
            sa.sa_flags = libc::SA_RESTART;
            libc::sigemptyset(&mut sa.sa_mask);
            libc::sigaction(s, &sa, std::ptr::null_mut());
        }
    }
    fds[0]
}

/// signal.Stop: the default action again.
pub fn reset(sigs: &[libc::c_int]) {
    for &s in sigs {
        unsafe {
            libc::signal(s, libc::SIG_DFL);
        }
    }
}

/// Blocks for the next signal number; None when the pipe fails.
pub fn next(fd: i32) -> Option<libc::c_int> {
    let mut b = 0u8;
    loop {
        let n = unsafe { libc::read(fd, &mut b as *mut u8 as *mut libc::c_void, 1) };
        if n == 1 {
            return Some(b as libc::c_int);
        }
        if n < 0 && crate::util::last_errno() == libc::EINTR {
            continue;
        }
        return None;
    }
}

/// wait4 with WUNTRACED, retried on EINTR.
pub fn wait_untraced(pid: i32) -> Result<i32, ()> {
    loop {
        let mut st: libc::c_int = 0;
        let r = unsafe { libc::wait4(pid, &mut st, libc::WUNTRACED, std::ptr::null_mut()) };
        if r < 0 {
            if crate::util::last_errno() == libc::EINTR {
                continue;
            }
            return Err(());
        }
        return Ok(st);
    }
}

/// Raw wait-status decoding as Go does it, so SIGSTOP stops are seen on BSD too.
pub fn stopped(st: i32) -> bool {
    st & 0x7f == 0x7f
}

pub fn signaled(st: i32) -> Option<i32> {
    let m = st & 0x7f;
    if m != 0x7f && m != 0 {
        Some(m)
    } else {
        None
    }
}

pub fn exited(st: i32) -> Option<i32> {
    if st & 0x7f == 0 {
        Some((st >> 8) & 0xff)
    } else {
        None
    }
}

/// Exit code for a terminal wait status: the status, or 128+signal.
pub fn code_of(st: i32) -> Option<i32> {
    if let Some(s) = signaled(st) {
        return Some(128 + s);
    }
    exited(st)
}
