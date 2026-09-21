#!/usr/bin/env bash
# [INPUT]: 接收生产 SQLite 数据库副本文件路径参数
# [OUTPUT]: 在临时安全沙箱中执行副本迁移并打印 11 项一致性自检报告
# [POS]: scripts/ 生产发布数据库副本只读验收脚本
# [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

# ==============================================================================
# 跨平台运行说明 (Cross-Platform Notes):
# - Linux / macOS: 直接执行 `bash scripts/release-validate-db.sh /path/to/source.db`
# - Windows:
#     1) 在 Git Bash / WSL 下直接执行 `bash scripts/release-validate-db.sh path/to/source.db`
#     2) 在 PowerShell / CMD 下直接执行 `go run ./scripts/validate_db.go path\to\source.db`
# ==============================================================================

set -euo pipefail

SOURCE_DB="${1:-}"

if [[ -z "$SOURCE_DB" ]]; then
    echo "Usage (Linux/macOS): $0 <source_database_file>"
    echo "Usage (Windows PS) : go run ./scripts/validate_db.go <source_database_file>"
    echo "Example: $0 ./data/icloud_hme.db"
    exit 2
fi

if [[ ! -f "$SOURCE_DB" ]]; then
    echo "[ERROR] Source database file '$SOURCE_DB' does not exist or is not a regular file."
    exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "=== icloud-hme Release DB Validation Runner ==="
echo "Target Source DB: $SOURCE_DB"
echo "Repository Root:  $REPO_ROOT"
echo ""

cd "$REPO_ROOT"

if command -v go >/dev/null 2>&1; then
    go run ./scripts/validate_db.go "$SOURCE_DB"
else
    echo "[ERROR] 'go' command is required to run the authoritative migration validation engine."
    echo "[HINT] On Linux/macOS, ensure Go is installed and on PATH."
    echo "[HINT] On Windows, you can also run directly in PowerShell: go run ./scripts/validate_db.go \"$SOURCE_DB\""
    exit 1
fi
