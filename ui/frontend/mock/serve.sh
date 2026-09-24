#!/bin/sh
# Serves the frontend with a mocked Go backend for UI work without a node.
# Usage: ui/frontend/mock/serve.sh   then open http://127.0.0.1:8765/
# Query parameters: ?hasNode=1  ?scenario=channels|pending|onchain
#   ?slow=list|restore|sync|addresses|rescan1|rescan2|channels (holds that stage)
set -e
cd "$(dirname "$0")"
rm -rf out && mkdir out
cp ../dist/style.css ../dist/app.js ../dist/logo.svg mock.js out/
sed 's#<script src="app.js"></script>#<script src="mock.js"></script><script src="app.js"></script>#' ../dist/index.html > out/index.html
cd out && python3 -m http.server 8765 --bind 127.0.0.1
