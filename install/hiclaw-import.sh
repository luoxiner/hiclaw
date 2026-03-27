#!/bin/bash
# hiclaw-import.sh - Import Worker/Team/Human resources into HiClaw
#
# Thin shell that delegates to the `hiclaw` CLI inside the Manager container.
# Supports legacy ZIP packages, Nacos AgentSpec URIs, and declarative YAML files.
#
# Usage:
#   ./hiclaw-import.sh --zip <path-or-url> --name <worker-name> [--yes]
#   ./hiclaw-import.sh --nacos <nacos-uri> [--name <worker-name>] [--yes]
#   ./hiclaw-import.sh -f <resource.yaml> [--prune] [--dry-run]
#
# Environment variables (for automation):
#   HICLAW_IMPORT_ZIP            Path or URL to Worker package ZIP
#   HICLAW_NON_INTERACTIVE       Skip all prompts (same as --yes)

set -e

# ============================================================
# Helper functions
# ============================================================

# normalize_name - Lowercase, strip non-alphanumeric except hyphens,
#                  trim leading/trailing hyphens.
normalize_name() {
    local input="$1"
    # Lowercase
    input=$(printf '%s' "$input" | tr '[:upper:]' '[:lower:]')
    # Remove characters that are not alphanumeric or hyphens
    input=$(printf '%s' "$input" | tr -cd 'a-z0-9-')
    # Trim leading hyphens
    input=$(printf '%s' "$input" | sed 's/^-*//')
    # Trim trailing hyphens
    input=$(printf '%s' "$input" | sed 's/-*$//')
    printf '%s' "$input"
}

# ============================================================
# Parse arguments
# ============================================================

ZIP_FILE="${HICLAW_IMPORT_ZIP:-}"
NACOS_URI=""
WORKER_NAME="${HICLAW_IMPORT_WORKER_NAME:-}"
AUTO_YES="${HICLAW_NON_INTERACTIVE:-0}"

while [ $# -gt 0 ]; do
    case "$1" in
        --zip)   ZIP_FILE="$2"; shift 2 ;;
        --nacos) NACOS_URI="$2"; shift 2 ;;
        --name)  WORKER_NAME="$2"; shift 2 ;;
        --yes)  AUTO_YES=1; shift ;;
        -f|--file)
            # YAML declarative mode — delegate to hiclaw-apply.sh
            SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
            exec bash "${SCRIPT_DIR}/hiclaw-apply.sh" -f "$2" "${@:3}"
            ;;
        -h|--help)
            echo "Usage: $0 --zip <path-or-url> --name <worker-name> [--yes]"
            echo "       $0 --nacos <nacos-uri> [--name <worker-name>] [--yes]"
            echo "       $0 -f <resource.yaml> [--prune] [--dry-run]  (declarative YAML mode)"
            echo ""
            echo "Nacos URI format: nacos://{instance-id}/{namespace}/{agentspec-name}[/{version}]"
            echo "  Requires HICLAW_NACOS_ADDR environment variable (format: [user:pass@]host:port)"
            exit 0 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Mutual exclusivity: --nacos and --zip cannot both be provided
if [ -n "${NACOS_URI}" ] && [ -n "${ZIP_FILE}" ]; then
    echo "ERROR: --nacos and --zip are mutually exclusive; provide one or the other" >&2
    exit 1
fi

# Validate required arguments depending on mode
if [ -n "${NACOS_URI}" ]; then
    # nacos mode: --name is optional (will be derived from URI in a later task)
    # Validate nacos:// URI prefix
    case "${NACOS_URI}" in
        nacos://*)
            ;; # valid prefix
        *)
            echo "ERROR: Invalid nacos URI: must start with nacos:// (expected format: nacos://{instance-id}/{namespace}/{agentspec-name}[/{version}])" >&2
            exit 1
            ;;
    esac

    # Extract worker name from URI if --name was not provided
    if [ -z "${WORKER_NAME}" ]; then
        # URI path after nacos://{host} is /{namespace}/{agentspec-name}[/{version}]
        # Strip scheme+host to get the path, then extract the 2nd path segment (agentspec-name)
        URI_PATH="${NACOS_URI#nacos://}"      # remove scheme
        URI_PATH="${URI_PATH#*/}"             # remove host → namespace/agentspec[/version]
        WORKER_NAME="${URI_PATH#*/}"          # remove namespace → agentspec[/version]
        WORKER_NAME="${WORKER_NAME%%/*}"      # remove /version if present
    fi

    # Normalize worker name (lowercase, alphanumeric + hyphens, trim hyphens)
    WORKER_NAME=$(normalize_name "${WORKER_NAME}")

    # Ensure we have a valid worker name after normalization
    if [ -z "${WORKER_NAME}" ]; then
        echo "ERROR: Could not derive a valid worker name from the nacos URI or --name argument" >&2
        exit 1
    fi

    # Verify HICLAW_NACOS_ADDR is set (required for nacos:// imports)
    if [ -z "${HICLAW_NACOS_ADDR:-}" ]; then
        echo "ERROR: HICLAW_NACOS_ADDR environment variable is required for nacos:// imports (format: [user:pass@]host:port)" >&2
        exit 1
    fi

    # Generate minimal Worker YAML to a temp file
    NACOS_YAML_TMP=$(mktemp /tmp/hiclaw-nacos-worker-XXXXXX)
    trap 'rm -f "${NACOS_YAML_TMP}"' EXIT

    cat > "${NACOS_YAML_TMP}" <<EOF
apiVersion: hiclaw.io/v1
kind: Worker
metadata:
  name: ${WORKER_NAME}
spec:
  package: ${NACOS_URI}
EOF

    # Display import summary
    echo ""
    echo "[HiClaw Import] Nacos Import Summary"
    echo "  Nacos URI:    ${NACOS_URI}"
    echo "  Worker name:  ${WORKER_NAME}"
    echo ""

    # Prompt for confirmation unless --yes is set
    if [ "${AUTO_YES}" != "1" ]; then
        printf "Proceed with import? [y/N] "
        read -r CONFIRM
        case "${CONFIRM}" in
            [yY]|[yY][eE][sS]) ;;
            *)
                echo "Import cancelled."
                exit 0
                ;;
        esac
    fi

    # --- Delegate nacos import to container-internal hiclaw CLI ---

    # Detect container runtime
    CONTAINER_CMD=""
    if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
        CONTAINER_CMD="docker"
    elif command -v podman &>/dev/null && podman info &>/dev/null 2>&1; then
        CONTAINER_CMD="podman"
    fi
    if [ -z "${CONTAINER_CMD}" ]; then
        echo "ERROR: Neither docker nor podman found" >&2
        exit 1
    fi

    # Verify Manager container
    if ! ${CONTAINER_CMD} ps --filter name=hiclaw-manager --format '{{.Names}}' 2>/dev/null | grep -q 'hiclaw-manager'; then
        echo "ERROR: hiclaw-manager container is not running" >&2
        exit 1
    fi

    # Copy YAML into container at /tmp/import/
    NACOS_YAML_BASENAME=$(basename "${NACOS_YAML_TMP}")
    ${CONTAINER_CMD} exec hiclaw-manager mkdir -p /tmp/import 2>/dev/null || true
    ${CONTAINER_CMD} cp "${NACOS_YAML_TMP}" "hiclaw-manager:/tmp/import/${NACOS_YAML_BASENAME}"
    echo "[HiClaw Import] Copied ${NACOS_YAML_BASENAME} → container:/tmp/import/"

    # Delegate to hiclaw apply -f inside container with HICLAW_NACOS_ADDR env passthrough
    HICLAW_ARGS=("apply" "-f" "/tmp/import/${NACOS_YAML_BASENAME}")
    if [ "${AUTO_YES}" = "1" ]; then
        HICLAW_ARGS+=("--yes")
    fi

    exec ${CONTAINER_CMD} exec -e "HICLAW_NACOS_ADDR=${HICLAW_NACOS_ADDR}" hiclaw-manager hiclaw "${HICLAW_ARGS[@]}"

elif [ -z "${ZIP_FILE}" ] || [ -z "${WORKER_NAME}" ]; then
    echo "Usage: $0 --zip <path-or-url> --name <worker-name> [--yes]"
    echo "       $0 --nacos <nacos-uri> [--name <worker-name>] [--yes]"
    echo "       $0 -f <resource.yaml> [--prune] [--dry-run]  (declarative YAML mode)"
    exit 1
fi

# ============================================================
# Delegate --zip to container-internal hiclaw CLI
# ============================================================

# Detect container runtime
CONTAINER_CMD=""
if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
    CONTAINER_CMD="docker"
elif command -v podman &>/dev/null && podman info &>/dev/null 2>&1; then
    CONTAINER_CMD="podman"
fi
if [ -z "${CONTAINER_CMD}" ]; then
    echo "ERROR: Neither docker nor podman found" >&2
    exit 1
fi

# Verify Manager container
if ! ${CONTAINER_CMD} ps --filter name=hiclaw-manager --format '{{.Names}}' 2>/dev/null | grep -q 'hiclaw-manager'; then
    echo "ERROR: hiclaw-manager container is not running" >&2
    exit 1
fi

# Handle URL: download ZIP first
if echo "${ZIP_FILE}" | grep -qE '^https?://'; then
    echo "[HiClaw Import] Downloading ${ZIP_FILE}..."
    DOWNLOADED_ZIP=$(mktemp /tmp/hiclaw-import-XXXXXX.zip)
    curl -fSL -o "${DOWNLOADED_ZIP}" "${ZIP_FILE}" || { echo "ERROR: Download failed"; exit 1; }
    ZIP_FILE="${DOWNLOADED_ZIP}"
    trap 'rm -f "${DOWNLOADED_ZIP}"' EXIT
fi

# Copy ZIP into container
ZIP_BASENAME=$(basename "${ZIP_FILE}")
${CONTAINER_CMD} exec hiclaw-manager mkdir -p /tmp/import 2>/dev/null || true
${CONTAINER_CMD} cp "${ZIP_FILE}" "hiclaw-manager:/tmp/import/${ZIP_BASENAME}"
echo "[HiClaw Import] Copied ${ZIP_BASENAME} → container:/tmp/import/"

# Delegate to hiclaw apply --zip --name inside container
HICLAW_ARGS=("apply" "--zip" "/tmp/import/${ZIP_BASENAME}" "--name" "${WORKER_NAME}")
if [ "${AUTO_YES}" = "1" ]; then
    HICLAW_ARGS+=("--yes")
fi

exec ${CONTAINER_CMD} exec hiclaw-manager hiclaw "${HICLAW_ARGS[@]}"
