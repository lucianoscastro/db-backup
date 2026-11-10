#!/bin/bash
# Restaura uma base a partir de um backup local ou do S3.
#
#   restore <engine> <db> [arquivo]   arquivo omitido = backup local mais recente
#   restore <engine> <db> --from-s3   baixa a key mais recente de <prefix>/<engine>/<db>/
#   restore mongo <db> ... --drop     (só mongo) apaga as coleções antes de restaurar
#
# A resolução do arquivo é genérica; o restore em si é delegado ao driver.
# Atenção: a credencial de backup costuma ter só s3:PutObject — o download exige
# uma credencial com GetObject/ListBucket (ver README).
set -euo pipefail
source /usr/local/bin/s3.sh

BACKUP_DIR="${BACKUP_DIR:-/backups}"
DRIVERS_DIR="${DRIVERS_DIR:-/usr/local/bin/drivers}"

engine="${1:-}"
db="${2:-}"
[ -n "$engine" ] && [ -n "$db" ] || {
    echo "uso: restore <engine> <db> [arquivo|--from-s3] [--drop]" >&2
    echo "engines: $(cd "$DRIVERS_DIR" && ls -- *.sh | sed 's/\.sh$//' | tr '\n' ' ')" >&2
    exit 1
}
shift 2

drv="$DRIVERS_DIR/$engine.sh"
[ -x "$drv" ] || { echo "[Restore] ERRO: engine desconhecido: $engine" >&2; exit 1; }
"$drv" enabled || { echo "[Restore] ERRO: engine $engine não configurado." >&2; exit 1; }

source_arg=""
extra_args=()
for arg in "$@"; do
    case "$arg" in
        --drop) extra_args+=(--drop) ;;
        *) source_arg="$arg" ;;
    esac
done

archive=""
if [ "$source_arg" = "--from-s3" ]; then
    s3_enabled || { echo "[Restore] ERRO: S3 não configurado (S3_ENABLED/S3_BUCKET)." >&2; exit 1; }

    key=$(s3_list "$(s3_key "$engine/$db/")" | sort | tail -1)
    [ -n "$key" ] || { echo "[Restore] ERRO: nenhum backup de '$db' no S3." >&2; exit 1; }

    archive="/tmp/$(basename "$key")"
    # sem exec no fim: a cópia baixada não pode sobrar no container
    trap 'rm -f "$archive"' EXIT
    echo "[Restore] Baixando s3://${S3_BUCKET}/${key}"
    s3_download "$key" "$archive"
elif [ -n "$source_arg" ]; then
    archive="$source_arg"
    [ -f "$archive" ] || archive="$BACKUP_DIR/$engine/$db/$source_arg"
else
    archive=$(ls -1t "$BACKUP_DIR/$engine/$db/${db}_"* 2>/dev/null | head -1 || true)
    [ -n "$archive" ] || { echo "[Restore] ERRO: nenhum backup local de '$db' em $BACKUP_DIR/$engine/$db." >&2; exit 1; }
fi

[ -f "$archive" ] || { echo "[Restore] ERRO: arquivo não encontrado: $archive" >&2; exit 1; }

"$drv" restore "$db" "$archive" ${extra_args[@]+"${extra_args[@]}"}
