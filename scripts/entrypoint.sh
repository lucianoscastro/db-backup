#!/bin/bash
set -euo pipefail

# Backups são o banco inteiro: só o dono (uid 1000) lê, inclusive no volume do host.
umask 077

INTERVAL_HOURS="${INTERVAL_HOURS:-6}"
RETRY_MINUTES="${RETRY_MINUTES:-15}"
RUN_ON_START="${RUN_ON_START:-true}"

# Aceita fração (0.5 = 30 min); o sleep precisa de segundos.
to_seconds() { awk "BEGIN{printf \"%d\", $1 * $2}"; }

case "${1:-serve}" in
    # UI web + agendador no mesmo processo (binário Go). Modo padrão.
    serve)
        exec backupd
        ;;
    # Agendador sem UI web, para quem não quer expor porta nenhuma.
    daemon)
        interval=$(to_seconds "$INTERVAL_HOURS" 3600)
        retry=$(to_seconds "$RETRY_MINUTES" 60)
        echo "[Daemon] Intervalo: ${INTERVAL_HOURS}h (retry em ${RETRY_MINUTES}min se falhar)."

        [ "${RUN_ON_START,,}" = "true" ] || sleep "$interval"

        while true; do
            if backup.sh; then
                sleep "$interval"
            else
                echo "[Daemon] ERRO: ciclo falhou. Nova tentativa em ${RETRY_MINUTES}min." >&2
                sleep "$retry"
            fi
        done
        ;;
    backup)  shift; exec backup.sh "$@" ;;
    restore) shift; exec restore.sh "$@" ;;
    list)    find "${BACKUP_DIR:-/backups}" -type f \( -name '*.tar' -o -name '*.bak' \) -exec ls -lh {} + ;;
    *)       exec "$@" ;;
esac
