#!/bin/sh
# Called by tmux after-rename-session/after-rename-window hooks.
# Collects pane IDs and POSTs to the lifecycle endpoint.
#
# Usage:
#   tmux_rename_hook.sh <endpoint_url> <new_name> [session_name] [list-panes-flags]
#
# Examples:
#   tmux_rename_hook.sh http://host:port/hooks/lifecycle/session-renamed "newsess" "" "-s"
#   tmux_rename_hook.sh http://host:port/hooks/lifecycle/window-renamed "newwin" "sess" ""

url="$1"
new_name="$2"
session_name="$3"
pane_flags="${4:--s}"

panes=$(tmux list-panes $pane_flags -F '#{pane_id}' \
  | awk '{printf "\"%s\",", $0}' \
  | sed 's/,$//')

if [ -n "$session_name" ]; then
  json="{\"session_name\":\"$session_name\",\"new_name\":\"$new_name\",\"pane_ids\":[$panes]}"
else
  json="{\"new_name\":\"$new_name\",\"pane_ids\":[$panes]}"
fi

curl -sf -X POST "$url" -H "Content-Type: application/json" -d "$json"
