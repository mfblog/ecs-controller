#!/usr/bin/env bash
set -Eeuo pipefail

REPO_URL="${REPO_URL:-https://github.com/Kori1c/ecs-controller.git}"
INSTALL_DIR="${INSTALL_DIR:-${HOME}/ecs-controller}"
BRANCH="${BRANCH:-main}"
deploy_args=()

usage() {
    cat <<'EOF'
Usage: install.sh [--dir PATH] [--branch NAME] [--no-build]

Clone or update ecs-controller, then pull prebuilt images and deploy it with Docker Compose.

Options:
  --dir PATH       Installation directory (default: $HOME/ecs-controller)
  --branch NAME    Git branch to install (default: main)
  --no-build       Compatibility option; images are never built on the deployment host
  -h, --help       Show this help

Environment:
  ECS_SETUP_TOKEN  One-time token used during first-run initialization.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dir)
            [[ $# -ge 2 ]] || { echo "--dir requires a path." >&2; exit 2; }
            INSTALL_DIR="$2"
            shift 2
            ;;
        --branch)
            [[ $# -ge 2 ]] || { echo "--branch requires a name." >&2; exit 2; }
            BRANCH="$2"
            shift 2
            ;;
        --no-build)
            deploy_args+=(--no-build)
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

if ! command -v git >/dev/null 2>&1; then
    echo "Git is required. Install Git and run this script again." >&2
    exit 1
fi

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
if [[ -f "$script_dir/deploy.sh" && -d "$script_dir/.git" ]]; then
    project_dir="$script_dir"
elif [[ -d "$INSTALL_DIR/.git" ]]; then
    project_dir="$INSTALL_DIR"
    echo "Updating $project_dir..."
    git -C "$project_dir" fetch origin "$BRANCH"
    git -C "$project_dir" pull --ff-only origin "$BRANCH"
else
    if [[ -e "$INSTALL_DIR" ]]; then
        echo "Install directory exists but is not an ecs-controller Git repository: $INSTALL_DIR" >&2
        exit 1
    fi
    echo "Cloning ecs-controller into $INSTALL_DIR..."
    git clone --depth 1 --branch "$BRANCH" "$REPO_URL" "$INSTALL_DIR"
    project_dir="$INSTALL_DIR"
fi

if ! command -v docker >/dev/null 2>&1; then
    echo "Docker is required. Install Docker Desktop or Colima, then run this script again." >&2
    exit 1
fi

if ! docker info >/dev/null 2>&1; then
    if command -v colima >/dev/null 2>&1; then
        echo "Docker is not running; starting Colima..."
        colima start
    else
        echo "Docker is not running. Start Docker Desktop or Colima, then run this script again." >&2
        exit 1
    fi
fi

cd "$project_dir"
exec ./deploy.sh "${deploy_args[@]}"
