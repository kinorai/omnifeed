# omnifeed demo tape — renders the README's assets/demo.gif
#
# Render:
#   1. install VHS:  https://github.com/charmbracelet/vhs  (needs ffmpeg + ttyd)
#   2. bring the local stack up (runs with OMNIFEED_DEV_NO_AUTH=true, so no token):
#        curl -fsSL https://raw.githubusercontent.com/kinorai/omnifeed/main/docker-compose.yml -o docker-compose.yml
#        curl -fsSL --create-dirs https://raw.githubusercontent.com/kinorai/omnifeed/main/searxng/settings.yml -o searxng/settings.yml
#        docker compose up -d
#   3. from the repo root:  vhs assets/demo.tape   ->   assets/demo.gif
#
# Tip: swap the Reddit URL below for a real thread to record a fuller comment tree.

Require curl
Require jq

Output assets/demo.gif

Set Shell bash
Set FontSize 18
Set Width 1280
Set Height 760
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 55ms

Hide
Type "clear"
Enter
Show

Sleep 600ms
Type "# 1) search the web via SearXNG  —  query -> ranked URLs"
Enter
Sleep 800ms
Type "curl -s localhost:8080/search -H \"content-type: application/json\" -d '{\"query\":\"go generics tutorial\",\"limit\":3}' | jq -r '.[].url'"
Enter
Sleep 3500ms

Type "# 2) fetch a Reddit thread  ->  full comment tree as TOON (no API key)"
Enter
Sleep 800ms
Type "curl -s 'localhost:8080/crawl?format=toon&expand=2' -H \"content-type: application/json\" -d '{\"urls\":[\"https://www.reddit.com/r/golang/comments/1abcd/generics/\"]}' | jq -r '.[0].page_content' | head -28"
Enter
Sleep 4500ms

Type "# one gateway: MCP + Open WebUI + REST, one Go binary."
Enter
Sleep 2500ms
