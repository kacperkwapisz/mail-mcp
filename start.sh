#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

# ── OTA update ────────────────────────────────────────────────────────────────
# Uses curl + Python stdlib tarfile — no git or unzip required.
#
# Flow:
#   1. Fetch latest GitHub Release tag via API (tiny JSON, 5 s timeout).
#   2. Compare against .poke_version (last installed version tag). Skip if
#      already up to date or if the remote is unreachable.
#   3. Download the release tarball only when an update exists (30 s timeout).
#   4. Extract with Python tarfile, stripping the GitHub top-level prefix and
#      skipping protected local files (.env, config.yml, .venv).
#   5. Persist new version tag to .poke_version and reinstall deps if
#      requirements.txt changed.

if command -v curl &>/dev/null && command -v python3 &>/dev/null; then
  _OTA_REPO="kacperkwapisz/poke-mail"
  _VERSION_FILE=".poke_version"

  echo "Checking for updates..."

  # Step 1: fetch latest release tag and tarball URL (fail silently if offline)
  _RELEASE_JSON=$(curl -sf --max-time 5 \
    "https://api.github.com/repos/${_OTA_REPO}/releases/latest" \
    2>/dev/null || echo "")

  _REMOTE_TAG=""
  _TARBALL_URL=""
  if [ -n "$_RELEASE_JSON" ]; then
    _REMOTE_TAG=$(echo "$_RELEASE_JSON" | python3 -c \
      "import json,sys; print(json.load(sys.stdin).get('tag_name',''))" \
      2>/dev/null || echo "")
    _TARBALL_URL=$(echo "$_RELEASE_JSON" | python3 -c \
      "import json,sys; print(json.load(sys.stdin).get('tarball_url',''))" \
      2>/dev/null || echo "")
  fi

  _LOCAL_TAG=$(cat "$_VERSION_FILE" 2>/dev/null || echo "")

  if [ -z "$_REMOTE_TAG" ]; then
    echo "  ℹ  Could not reach remote — continuing with local version."
    echo ""
  elif [ "$_REMOTE_TAG" = "$_LOCAL_TAG" ]; then
    echo "  ✓ Already up to date (${_LOCAL_TAG})"
    echo ""
  else
    if [ -n "$_LOCAL_TAG" ]; then
      echo "  ↳ Update found (${_LOCAL_TAG} → ${_REMOTE_TAG}), downloading..."
    else
      echo "  ↳ Update found (${_REMOTE_TAG}), downloading..."
    fi

    # Hash requirements.txt before extraction so we can detect changes
    _REQS_BEFORE=$(python3 -c \
      "import hashlib; print(hashlib.md5(open('requirements.txt','rb').read()).hexdigest())" \
      2>/dev/null || echo "")

    _TMP_TAR=$(mktemp /tmp/poke-mail-update.XXXXXX.tar.gz)

    if curl -sfL --max-time 30 \
         "$_TARBALL_URL" \
         -o "$_TMP_TAR" 2>/dev/null; then

      # Extract with Python: strip GitHub's top-level dir, skip protected paths
      python3 - "$_TMP_TAR" <<'PYEOF'
import sys, tarfile, os

archive = sys.argv[1]
# Files/dirs that must never be overwritten by an OTA update
PROTECTED = {'.env', 'config.yml', '.venv', '.poke_version'}

try:
    with tarfile.open(archive, 'r:gz') as tf:
        members = tf.getmembers()
        if not members:
            sys.exit(0)
        # GitHub tarball root dir is e.g. "owner-repo-<sha>/"
        prefix = members[0].name.split('/')[0] + '/'
        for m in members:
            if not m.name.startswith(prefix):
                continue
            rel = m.name[len(prefix):]   # path relative to repo root
            if not rel:                   # skip the root dir entry itself
                continue
            top = rel.split('/')[0]
            if top in PROTECTED:
                continue
            m.name = rel
            try:
                tf.extract(m, path='.', set_attrs=False)
            except Exception:
                pass  # best-effort; don't abort on permission issues etc.
except Exception as e:
    print(f'  ⚠  Extraction error: {e}')
    sys.exit(1)
PYEOF

      # Persist new version tag so we don't re-download next run
      echo "$_REMOTE_TAG" > "$_VERSION_FILE"
      echo "  ✓ Updated to ${_REMOTE_TAG}"

      # Reinstall deps if requirements.txt changed
      _REQS_AFTER=$(python3 -c \
        "import hashlib; print(hashlib.md5(open('requirements.txt','rb').read()).hexdigest())" \
        2>/dev/null || echo "")
      if [ -n "$_REQS_BEFORE" ] && [ "$_REQS_BEFORE" != "$_REQS_AFTER" ]; then
        echo "  ↳ requirements.txt changed — reinstalling dependencies..."
        [ -d .venv ] && source .venv/bin/activate
        pip install -q -r requirements.txt
        echo "  ✓ Dependencies updated"
      fi
    else
      echo "  ℹ  Download failed — continuing with local version."
    fi

    rm -f "$_TMP_TAR"
    echo ""
  fi
fi

# ── One-time setup (skipped on subsequent runs) ───────────────────────────────

# 1. Python virtualenv + dependencies
if [ ! -d .venv ]; then
  echo "First run — setting up poke-mail..."
  echo ""
  echo "Creating Python virtualenv (.venv)..."
  python3 -m venv .venv
fi
source .venv/bin/activate

if ! python3 -c "import fastmcp" &>/dev/null 2>&1; then
  echo "Installing Python dependencies..."
  pip install -q -r requirements.txt
  echo "  ✓ Dependencies installed"
  echo ""
fi

# 2. config.yml — copy example if missing
if [ ! -f config.yml ]; then
  echo "Copying config.example.yml → config.yml..."
  cp config.example.yml config.yml
  echo "  ✓ config.yml created"
  echo ""
fi

# 3. Poke API key — read from 'poke login' credentials, inject into config.yml
#    The 'poke' npm package writes ~/.config/poke/credentials.json { "token": "..." }
#    after 'npx poke login'. We auto-read it so the user never has to copy/paste.
if grep -q 'your-api-key-here' config.yml 2>/dev/null; then
  POKE_CREDENTIALS_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/poke/credentials.json"
  POKE_TOKEN=""

  if [ -f "$POKE_CREDENTIALS_FILE" ]; then
    POKE_TOKEN=$(python3 -c "
import json, sys
try:
    data = json.load(open('$POKE_CREDENTIALS_FILE'))
    print(data.get('token', ''))
except Exception:
    print('')
" 2>/dev/null || true)
  fi

  if [ -n "$POKE_TOKEN" ]; then
    echo "  ✓ Poke API key detected from 'poke login'"
    POKE_TOKEN="$POKE_TOKEN" python3 - <<'PYEOF'
import os, re, json
token = os.environ['POKE_TOKEN']
with open('config.yml', 'r') as f:
    content = f.read()
pattern = r'(?m)^([ \t]*poke_api_key:[ \t*])[^\n]+'
new_content, n = re.subn(pattern, lambda m: m.group(1) + json.dumps(token), content)
if n == 0:
    print('  ⚠  Warning: poke_api_key key not found in config.yml — update it manually.')
with open('config.yml', 'w') as f:
    f.write(new_content)
PYEOF
    echo "  ✓ poke_api_key written to config.yml"
    echo ""
  else
    echo "  ⚠  poke_api_key not set in config.yml."
    echo "     Run 'npx poke login' then restart, or paste your key from poke.com/settings/advanced"
    echo ""
    printf "  Poke API key (leave blank to set manually later): "
    read -r POKE_TOKEN_INPUT
    POKE_TOKEN_INPUT=$(echo "$POKE_TOKEN_INPUT" | tr -d '[:space:]')
    if [ -n "$POKE_TOKEN_INPUT" ]; then
      POKE_TOKEN="$POKE_TOKEN_INPUT" python3 - <<'PYEOF'
import os, re, json
token = os.environ['POKE_TOKEN']
with open('config.yml', 'r') as f:
    content = f.read()
pattern = r'(?m)^([ \t]*poke_api_key:[ \t*])[^\n]+'
new_content, n = re.subn(pattern, lambda m: m.group(1) + json.dumps(token), content)
if n == 0:
    print('  ⚠  Warning: poke_api_key key not found in config.yml — update it manually.')
with open('config.yml', 'w') as f:
    f.write(new_content)
PYEOF
      echo "  ✓ poke_api_key saved to config.yml"
    fi
    echo ""
  fi
fi

# ── Load .env early so POKE_TUNNEL is visible to the MCP_API_KEY logic below ──
if [ -f .env ]; then
  set -a
  source .env
  set +a
fi

# Tunnel-mode detection must happen BEFORE the MCP_API_KEY block: in tunnel
# mode (POKE_TUNNEL=1, the default) the local server runs unauthenticated and
# the tunnel handles auth, so we must NOT generate a key — doing so previously
# overwrote user-supplied keys and broke working setups (issue #9).
POKE_TUNNEL="${POKE_TUNNEL:-1}"

# 4. MCP_API_KEY — generate once and persist to .env (skipped in tunnel mode)
if [ "${POKE_TUNNEL}" != "1" ] && { [ ! -f .env ] \
   || grep -Eq '^[[:space:]]*MCP_API_KEY=your-secret-key-here' .env 2>/dev/null \
   || ! grep -Eq '^[[:space:]]*MCP_API_KEY=.+' .env 2>/dev/null; }; then
  RANDOM_KEY=$(python3 -c "
import secrets, string
alphabet = string.ascii_letters + string.digits
print(''.join(secrets.choice(alphabet) for _ in range(48)))
")
  if [ -f .env ]; then
    RANDOM_KEY="$RANDOM_KEY" python3 - <<'PYEOF'
import os, re
new_key = os.environ['RANDOM_KEY']
with open('.env', 'r') as f:
    content = f.read()
new_content, n = re.subn(r'(?m)^[[:space:]]*MCP_API_KEY=.*', f'MCP_API_KEY={new_key}', content)
if n == 0:
    new_content += f'MCP_API_KEY={new_key}\n'
with open('.env', 'w') as f:
    f.write(new_content)
PYEOF
  else
    echo "MCP_API_KEY=${RANDOM_KEY}" > .env
  fi
  echo "  ✓ MCP_API_KEY generated and saved to .env"
  echo ""
  # Re-source so the freshly generated key is visible to the rest of the script
  set -a
  source .env
  set +a
fi

# ── Tunnel-mode enforcement ───────────────────────────────────────────────────
if [ "${POKE_TUNNEL}" != "1" ]; then
  : "${MCP_API_KEY:?MCP_API_KEY is not set — add it to .env or export it}"
else
  if [ -z "${MCP_API_KEY:-}" ]; then
    echo "  ℹ  MCP_API_KEY not set — server runs unauthenticated (safe: poke tunnel handles auth)."
  fi
export POKE_TUNNEL
fi

# ── Start server + tunnel ─────────────────────────────────────────────────────
echo "Starting poke-mail server..."
python3 src/server.py &
SERVER_PID=$!
trap "kill $SERVER_PID 2>/dev/null" EXIT

sleep 2

echo "Starting tunnel to Poke..."
if command -v poke &>/dev/null; then
  poke tunnel http://localhost:3000/mcp --name "poke-mail"
else
  echo "  ℹ  'poke' binary not found in PATH — using npx poke (requires Node.js)."
  if ! command -v npx &>/dev/null; then
    echo "  ✗ Neither 'poke' nor 'npx' found. Install Node.js (nodejs.org) and run:"
    echo "      npm install -g poke   OR   npx poke tunnel ..."
    exit 1
  fi
  npx --yes poke tunnel http://localhost:3000/mcp --name "poke-mail"
fi
