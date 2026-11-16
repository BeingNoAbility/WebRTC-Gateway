#!/usr/bin/env bash
# Bash script to sync local wiki documents to GitHub Wiki repository
# Usage: ./wiki/sync-wiki.sh [optional_wiki_git_url]

set -euo pipefail

WIKI_REPO_URL="${1:-https://github.com/BeingNoAbility/WebRTC-Gateway.wiki.git}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMP_DIR="$(mktemp -d -t webrtc-wiki-XXXXXX)"

echo "=========================================================="
echo " Syncing Wiki documentation to: ${WIKI_REPO_URL}"
echo "=========================================================="

echo "Checking if GitHub Wiki repository exists..."
if ! git ls-remote "${WIKI_REPO_URL}" > /dev/null 2>&1; then
    echo ""
    echo "[!] GitHub Wiki repository not initialized yet!"
    echo "    Reason: GitHub creates the .wiki.git repository only after you click"
    echo "    'Create the first page' on the web interface."
    echo ""
    echo "    Please follow these quick 2 steps:"
    echo "    1. Open: https://github.com/BeingNoAbility/WebRTC-Gateway/wiki in your browser"
    echo "    2. Click 'Create the first page', type anything, and click 'Save Page'."
    echo "    3. Run this script again!"
    echo ""
    exit 1
fi

cleanup() {
    rm -rf "${TEMP_DIR}"
}
trap cleanup EXIT

echo "Cloning wiki repository to temporary directory..."
git clone "${WIKI_REPO_URL}" "${TEMP_DIR}"

echo "Copying documentation pages..."
cp -f "${SCRIPT_DIR}"/*.md "${TEMP_DIR}/"
if [ -d "${SCRIPT_DIR}/assets" ]; then
    mkdir -p "${TEMP_DIR}/assets"
    cp -rf "${SCRIPT_DIR}/assets"/* "${TEMP_DIR}/assets/"
fi

cd "${TEMP_DIR}"
git config user.name "Wiki Bot"
git config user.email "wiki-bot@users.noreply.github.com"

git add -A
if git diff-index --quiet HEAD --; then
    echo "Wiki is already up to date. Nothing to commit."
else
    git commit -m "docs(wiki): update wiki documentation from wiki/ folder"
    git push origin master
    echo "Successfully pushed wiki documents to GitHub Wiki!"
fi
