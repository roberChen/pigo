// This file holds the Bash prelude sourced before the user's pigorc scripts: it
// defines the DSL builtins (option, hook) and the helpers that hand the result
// back to pigo (records on fd 3, generated hook wrappers, environment dump). It is
// assembled in Go so the hook-event list has a single source of truth
// (hooks.AllEvents) and cannot drift from the Go side.
package config

import (
	"fmt"
	"strings"

	"github.com/smallnest/pigo/internal/hooks"
)

// pigorcPrelude is the Bash DSL a pigorc builds config with.
var pigorcPrelude = buildPigorcPrelude()

// buildPigorcPrelude renders the prelude with the canonical event list baked in.
func buildPigorcPrelude() string {
	return fmt.Sprintf(`# pigo pigorc prelude (generated; do not edit)
# Records are one per line, fields separated by US (0x1f), written to fd 3 so a
# script's own stdout stays free for the user.
__pigo_emit() {
  local out="" arg
  for arg in "$@"; do
    arg=${arg//$'\r'/}
    arg=${arg//$'\n'/ }
    if [ -z "$out" ]; then
      out="$arg"
    else
      out="$out"$'\x1f'"$arg"
    fi
  done
  printf '%%s\n' "$out" >&3
}

# option <key> <value>   set a config option; option reset <key> clears a list
option() {
  if [ "${1:-}" = "reset" ]; then
    __pigo_emit option-reset "${2:-}"
    return 0
  fi
  if [ $# -lt 2 ]; then
    echo "pigorc: option needs a key and a value" >&2
    return 0
  fi
  local key="$1"
  shift
  __pigo_emit option "$key" "$*"
}

# hook add <event> [--matcher RE] [--timeout N] --command CMD
hook() {
  if [ "${1:-}" != "add" ]; then
    echo "pigorc: usage: hook add <event> [--matcher RE] [--timeout N] --command CMD" >&2
    return 0
  fi
  local event="${2:-}"
  shift 2 || true
  local matcher="" timeout="" command=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --matcher)
        matcher="${2:-}"; shift 2 || break ;;
      --timeout)
        timeout="${2:-}"; shift 2 || break ;;
      --command)
        command="${2:-}"; shift 2 || break ;;
      *)
        echo "pigorc: hook add: unknown argument $1" >&2
        shift ;;
    esac
  done
  if [ -z "$command" ]; then
    echo "pigorc: hook add $event: --command is required" >&2
    return 0
  fi
  __pigo_emit hook "$event" "$matcher" "$timeout" "$command"
}

# provider add <name> [--type P] [--base-url URL] [--env-var VAR] [--auth S]
#               [--api-key KEY] [--model ID ...]
# provider remove <name>
provider() {
  local action="${1:-}"
  shift || true
  case "$action" in
    add)
      local name="${1:-}"
      if [ -z "$name" ]; then
        echo "pigorc: provider add needs a name" >&2
        return 0
      fi
      shift || true
      local type="" base_url="" env_var="" auth="" api_key=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --type|--protocol)
            type="${2:-}"; shift 2 || break ;;
          --base-url)
            base_url="${2:-}"; shift 2 || break ;;
          --env-var)
            env_var="${2:-}"; shift 2 || break ;;
          --auth)
            auth="${2:-}"; shift 2 || break ;;
          --api-key)
            api_key="${2:-}"; shift 2 || break ;;
          --model)
            __pigo_emit providermodel "$name" "${2:-}"; shift 2 || break ;;
          *)
            echo "pigorc: provider add: unknown argument $1" >&2
            shift ;;
        esac
      done
      __pigo_emit provider add "$name" "$type" "$base_url" "$env_var" "$auth" "$api_key"
      ;;
    remove|rm)
      __pigo_emit provider remove "${1:-}"
      ;;
    *)
      echo "pigorc: usage: provider add <name> [--type openai|anthropic] [--base-url URL] [--env-var VAR] [--auth bearer|x-api-key] [--api-key KEY] [--model ID]..." >&2
      ;;
  esac
}

# agent add <name> [--description T] [--system-prompt T | --prompt-file F]
#                 [--tools a,b] [--model M] [--thinking L] [--isolation I]
#                 [--timeout N] [--inherit-hooks]
# agent hook <name> <event> [--matcher RE] [--timeout N] --command CMD
# agent remove <name>
agent() {
  local action="${1:-}"
  shift || true
  case "$action" in
    add)
      local name="${1:-}"
      if [ -z "$name" ]; then
        echo "pigorc: agent add needs a name" >&2
        return 0
      fi
      shift || true
      local description="" system_prompt="" tools="" model="" thinking="" isolation="" timeout="" inherit="false"
      while [ $# -gt 0 ]; do
        case "$1" in
          --description) description="${2:-}"; shift 2 || break ;;
          --system-prompt) system_prompt="${2:-}"; shift 2 || break ;;
          --prompt-file)
            if [ -n "${2:-}" ] && [ -f "$2" ]; then
              system_prompt="$(cat "$2")"
            else
              echo "pigorc: agent add $name: cannot read prompt file ${2:-}" >&2
            fi
            shift 2 || break ;;
          --tools) tools="${2:-}"; shift 2 || break ;;
          --model) model="${2:-}"; shift 2 || break ;;
          --thinking) thinking="${2:-}"; shift 2 || break ;;
          --isolation) isolation="${2:-}"; shift 2 || break ;;
          --timeout) timeout="${2:-}"; shift 2 || break ;;
          --inherit-hooks) inherit="true"; shift ;;
          *)
            echo "pigorc: agent add: unknown argument $1" >&2
            shift ;;
        esac
      done
      __pigo_emit agent add "$name" "$description" "$tools" "$model" "$thinking" "$isolation" "$timeout" "$inherit" "$system_prompt"
      ;;
    hook)
      local name="${1:-}"
      local event="${2:-}"
      shift 2 || true
      local matcher="" timeout="" command=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --matcher) matcher="${2:-}"; shift 2 || break ;;
          --timeout) timeout="${2:-}"; shift 2 || break ;;
          --command) command="${2:-}"; shift 2 || break ;;
          *)
            echo "pigorc: agent hook: unknown argument $1" >&2
            shift ;;
        esac
      done
      if [ -z "$command" ]; then
        echo "pigorc: agent hook $name $event: --command is required" >&2
        return 0
      fi
      __pigo_emit agenthook "$name" "$event" "$matcher" "$timeout" "$command"
      ;;
    remove|rm)
      __pigo_emit agent remove "${1:-}"
      ;;
    *)
      echo "pigorc: usage: agent add <name> ... | agent hook <name> <event> --command CMD | agent remove <name>" >&2
      ;;
  esac
}

# mcp add <name> [--command CMD] [--arg A ...] [--env K V ...]
#               [--url URL] [--header K V ...] [--timeout N]
#               [--transport http|sse] [--oauth] [--disabled]
# mcp remove <name>
mcp() {
  local action="${1:-}"
  shift || true
  case "$action" in
    add)
      local name="${1:-}"
      if [ -z "$name" ]; then
        echo "pigorc: mcp add needs a name" >&2
        return 0
      fi
      shift || true
      local command="" url="" timeout="" disabled="false" transport="" oauth="false"
      while [ $# -gt 0 ]; do
        case "$1" in
          --command)
            command="${2:-}"; shift 2 || break ;;
          --arg)
            __pigo_emit mcparg "$name" "${2:-}"; shift 2 || break ;;
          --env)
            __pigo_emit mcpenv "$name" "${2:-}" "${3:-}"; shift 3 || break ;;
          --url)
            url="${2:-}"; shift 2 || break ;;
          --header)
            __pigo_emit mcphdr "$name" "${2:-}" "${3:-}"; shift 3 || break ;;
          --timeout)
            timeout="${2:-}"; shift 2 || break ;;
          --transport)
            transport="${2:-}"; shift 2 || break ;;
          --oauth)
            oauth="true"; shift ;;
          --disabled)
            disabled="true"; shift ;;
          *)
            echo "pigorc: mcp add: unknown argument $1" >&2
            shift ;;
        esac
      done
      __pigo_emit mcp add "$name" "$command" "$url" "$timeout" "$disabled" "$transport" "$oauth"
      ;;
    remove|rm)
      __pigo_emit mcp remove "${1:-}"
      ;;
    *)
      echo "pigorc: usage: mcp add <name> [--command CMD] [--arg A ...] [--env K V ...] [--url URL] [--header K V ...] [--timeout N] [--transport http|sse] [--oauth] [--disabled] | mcp remove <name>" >&2
      ;;
  esac
}

# __pigo_env dumps the exported environment as NUL-separated name/value pairs so
# pigo can diff it and apply whatever the script exported.
__pigo_env() {
  local name
  for name in $(compgen -e); do
    printf '%%s\0%%s\0' "$name" "${!name}"
  done
}

# __pigo_match_event prints the canonical event name for a function name, or
# fails. Matching ignores case and separators (PreToolUse, pre_tool_use, ...).
__pigo_match_event() {
  local norm ev
  norm=$(printf '%%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -d '_-')
  for ev in %s; do
    if [ "$norm" = "$(printf '%%s' "$ev" | tr '[:upper:]' '[:lower:]')" ]; then
      printf '%%s' "$ev"
      return 0
    fi
  done
  return 1
}

# __pigo_emit_hook_functions turns every shell function named after a hook event
# into a command hook: the generated wrapper defines the function and calls it, so
# it receives the same stdin JSON and its exit code decides the outcome.
__pigo_emit_hook_functions() {
  local fn ev path
  for fn in $(compgen -A function); do
    case "$fn" in
      __pigo_*|option|hook) continue ;;
    esac
    if ! ev=$(__pigo_match_event "$fn"); then
      continue
    fi
    path="$PIGO_RC_HOOKS/$ev.$fn.sh"
    {
      printf '#!/usr/bin/env bash\n'
      declare -f "$fn"
      printf '%%s\n' "$fn"
    } > "$path"
    __pigo_emit hookfn "$ev" "$fn" "$path"
  done
}

__pigo_env > "$PIGO_RC_ENV_BEFORE"
`, strings.Join(hooks.AllEvents, " "))
}
