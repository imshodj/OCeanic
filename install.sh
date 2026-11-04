#!/bin/sh
# OCeanic RMM quick setup
#
#   curl -fsSL https://raw.githubusercontent.com/imshodj/OCeanic/main/install.sh | sh
#
# Creates ./oceanic (or $OCEANIC_DIR) with docker-compose.yml and .env, asks for
# your Cloudflare Tunnel token and public hostname, generates an admin
# password, then pulls and starts the containers. Safe to re-run: an existing
# .env is kept, and the images are updated.
#
# Non-interactive use: set CLOUDFLARE_TUNNEL_TOKEN and OCEANIC_PUBLIC_URL
# (and optionally OCEANIC_ADMIN_PASSWORD) in the environment.
set -eu

RAW="${OCEANIC_RAW:-https://raw.githubusercontent.com/imshodj/OCeanic/main}"
DIR="${OCEANIC_DIR:-$PWD/oceanic}"

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }
ask()  { # ask VAR "Prompt" [default]
	eval "cur=\${$1:-}"
	if [ -n "$cur" ]; then return; fi
	if [ ! -r /dev/tty ]; then die "$1 is not set and there's no terminal to ask on"; fi
	printf '%s%s: ' "$2" "${3:+ [$3]}" >/dev/tty
	read -r ans </dev/tty || true
	eval "$1=\${ans:-\${3:-}}"
}
setvar() { # setvar KEY VALUE  (rewrites KEY=... in .env without sed-escaping issues)
	awk -v k="$1" -v v="$2" 'BEGIN { done = 0 }
		index($0, k "=") == 1 { print k "=" v; done = 1; next } { print }
		END { if (!done) print k "=" v }' .env > .env.tmp && chmod 600 .env.tmp && mv .env.tmp .env
}

command -v docker >/dev/null 2>&1 || die "Docker isn't installed. See https://docs.docker.com/engine/install/"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (the 'docker compose' command)."
command -v curl >/dev/null 2>&1 || die "curl is required."

mkdir -p "$DIR"
cd "$DIR"
say "Installing into $DIR"
curl -fsSL "$RAW/docker-compose.yml" -o docker-compose.yml || die "couldn't download docker-compose.yml from $RAW (is the repository public?)"

if [ -f .env ]; then
	say "Keeping your existing .env"
else
	curl -fsSL "$RAW/.env.example" -o .env || die "couldn't download .env.example"
	chmod 600 .env
	echo
	echo "You need a Cloudflare Tunnel whose public hostname points to http://oceanic:8080"
	echo "(Zero Trust > Networks > Tunnels > Create a tunnel > Cloudflared)."
	echo
	ask CLOUDFLARE_TUNNEL_TOKEN "Cloudflare Tunnel token"
	ask OCEANIC_PUBLIC_URL "Public URL of the dashboard" "https://rmm.example.com"
	[ -n "$CLOUDFLARE_TUNNEL_TOKEN" ] || die "a tunnel token is required"
	case "$OCEANIC_PUBLIC_URL" in http://*|https://*) ;; *) OCEANIC_PUBLIC_URL="https://$OCEANIC_PUBLIC_URL" ;; esac
	OCEANIC_ADMIN_PASSWORD="${OCEANIC_ADMIN_PASSWORD:-$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 20)}"
	setvar CLOUDFLARE_TUNNEL_TOKEN "$CLOUDFLARE_TUNNEL_TOKEN"
	setvar OCEANIC_PUBLIC_URL "${OCEANIC_PUBLIC_URL%/}"
	setvar OCEANIC_ADMIN_PASSWORD "$OCEANIC_ADMIN_PASSWORD"
	NEW_PASSWORD="$OCEANIC_ADMIN_PASSWORD"
fi

if [ -n "${OCEANIC_NO_START:-}" ]; then
	say "Files are ready in $DIR (not starting, OCEANIC_NO_START is set)"
	exit 0
fi

say "Pulling images"
docker compose pull
say "Starting OCeanic"
docker compose up -d

URL="$(grep '^OCEANIC_PUBLIC_URL=' .env | cut -d= -f2-)"
echo
say "OCeanic is running."
echo "    Dashboard: $URL"
echo "    Username:  admin"
if [ -n "${NEW_PASSWORD:-}" ]; then
	echo "    Password:  $NEW_PASSWORD   (also saved in $DIR/.env)"
fi
echo
echo "    Logs:    cd $DIR && docker compose logs -f"
echo "    Update:  cd $DIR && docker compose pull && docker compose up -d"
