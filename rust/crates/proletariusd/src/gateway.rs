use std::env;
use std::ffi::OsString;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use nix::sys::signal::{Signal, kill};
use nix::unistd::Pid;
use proletarius_domain::BackendFailure;
use tokio::process::{Child, Command};
use tokio::time::timeout;

const GO_BACKEND_PATH_ENVIRONMENT: &str = "PROLETARIUS_GO_BACKEND_PATH";
// Go's bounded communication teardown is 65s (normally immediate). Do not
// kill the child while it is still restoring the user's radio policy.
const GO_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(70);

pub(crate) struct GoBackendProcess {
    child: Child,
    socket_path: PathBuf,
}

impl GoBackendProcess {
    pub(crate) fn spawn(
        public_socket_path: &Path,
        internal_token: &str,
    ) -> Result<Self, BackendFailure> {
        let executable = go_backend_path()?;
        let metadata = fs::metadata(&executable).map_err(|_| BackendFailure::GoBackendMissing)?;
        if !metadata.is_file() {
            return Err(BackendFailure::GoBackendMissing);
        }

        let socket_path = child_socket_path(public_socket_path);
        crate::prepare_socket_path(&socket_path)?;
        let mut command = Command::new(executable);
        command
            .arg("-listen")
            .arg(format!("unix:{}", socket_path.display()))
            .env("PROLETARIUS_CARRIER_SOCKET", public_socket_path)
            .env("SWITCH_INTERNAL_TOKEN", internal_token)
            .stdin(Stdio::null())
            .stdout(Stdio::inherit())
            .stderr(Stdio::inherit())
            .kill_on_drop(true);
        let child = command.spawn().map_err(crate::io_failure)?;
        Ok(Self { child, socket_path })
    }

    pub(crate) fn socket_path(&self) -> &Path {
        &self.socket_path
    }

    pub(crate) fn try_wait(&mut self) -> Result<Option<std::process::ExitStatus>, BackendFailure> {
        self.child.try_wait().map_err(crate::io_failure)
    }

    pub(crate) async fn terminate(mut self) -> Result<(), BackendFailure> {
        if self.child.try_wait().map_err(crate::io_failure)?.is_none() {
            if let Some(id) = self.child.id() {
                let pid = i32::try_from(id)
                    .map(Pid::from_raw)
                    .map_err(|_| BackendFailure::Io("Go backend PID is out of range".into()))?;
                kill(pid, Signal::SIGTERM)
                    .map_err(|error| BackendFailure::Io(error.to_string()))?;
            }
            if timeout(GO_SHUTDOWN_TIMEOUT, self.child.wait())
                .await
                .is_err()
            {
                self.child.kill().await.map_err(crate::io_failure)?;
                let _ = self.child.wait().await;
            }
        }
        let _ = fs::remove_file(&self.socket_path);
        Ok(())
    }
}

fn go_backend_path() -> Result<PathBuf, BackendFailure> {
    if let Some(path) = env::var_os(GO_BACKEND_PATH_ENVIRONMENT).filter(|value| !value.is_empty()) {
        return Ok(PathBuf::from(path));
    }
    let current = env::current_exe().map_err(crate::io_failure)?;
    current
        .parent()
        .map(|parent| parent.join("switchd"))
        .ok_or(BackendFailure::GoBackendMissing)
}

fn child_socket_path(public_socket_path: &Path) -> PathBuf {
    let mut value: OsString = public_socket_path.as_os_str().to_owned();
    value.push(".go");
    PathBuf::from(value)
}

#[cfg(test)]
mod tests {
    use super::child_socket_path;
    use std::path::Path;

    #[test]
    fn child_socket_never_aliases_public_socket() {
        let public = Path::new("/tmp/proletarius.sock");
        let child = child_socket_path(public);
        assert_eq!(child, Path::new("/tmp/proletarius.sock.go"));
        assert_ne!(child, public);
    }
}
