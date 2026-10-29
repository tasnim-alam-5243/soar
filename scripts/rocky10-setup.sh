#!/usr/bin/env bash
# One-time host setup for Rocky Linux 10.x: Docker CE + compose plugin, Go, git, make, kernel setting for OpenSearch.
# Run as root:  sudo ./scripts/rocky10-setup.sh      (idempotent; safe to re-run)
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
target_user="${SUDO_USER:-}"

echo "==> base packages"
dnf install -y dnf-plugins-core git make curl tar gcc golang

echo "==> Docker CE repository"
# Docker publishes RHEL packages that Rocky uses. If this repo ever lacks EL10 builds, use podman (see README).
if [[ ! -f /etc/yum.repos.d/docker-ce.repo ]]; then
  dnf config-manager --add-repo https://download.docker.com/linux/rhel/docker-ce.repo
fi
dnf install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker

if [[ -n "$target_user" ]]; then
  usermod -aG docker "$target_user"
  echo "==> added $target_user to the docker group (log out and back in for it to apply)"
fi

echo "==> OpenSearch needs vm.max_map_count >= 262144"
echo 'vm.max_map_count=262144' > /etc/sysctl.d/99-soar.conf
sysctl --system >/dev/null
sysctl vm.max_map_count

echo "==> versions"
docker --version
docker compose version
go version
echo "done. SELinux is left enforcing; the compose file uses named volumes and :z labels, so no changes are needed."
