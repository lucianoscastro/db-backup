#!/bin/bash
# Upload/download S3 via curl --aws-sigv4 (SigV4 nativo, sem aws-cli/mc).
#
# ponytail: usa PUT simples, cujo teto é 5 GB por objeto — arquivos maiores
# exigiriam multipart (upgrade: trocar por `mc cp`, +29 MB na imagem).
# s3_upload falha alto se o arquivo passar do teto, em vez de errar no servidor.

S3_MAX_PUT_BYTES=$((5 * 1024 * 1024 * 1024))

# Defaults obrigatórios: os scripts rodam com `set -u`, e uma destas variáveis
# ausente derrubaria o ciclo no meio (backup parcial e silencioso).
S3_ENABLED="${S3_ENABLED:-false}"
S3_BUCKET="${S3_BUCKET:-}"
S3_PREFIX="${S3_PREFIX:-}"
S3_REGION="${S3_REGION:-us-east-1}"
S3_ENDPOINT="${S3_ENDPOINT:-}"
AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-}"
AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-}"

s3_enabled() {
    [ "${S3_ENABLED,,}" = "true" ] && [ -n "$S3_BUCKET" ]
}

# Key completa a partir do caminho <engine>/<db>/<arquivo>. S3_PREFIX vazio
# (padrão) mantém o layout antigo da imagem mongo-only: mongo/<db>/<arquivo>.
s3_key() {
    echo "${S3_PREFIX:+${S3_PREFIX}/}$1"
}

# Endpoint no estilo path (bucket na URL): funciona igual em AWS, MinIO e R2.
s3_url() {
    local key="$1"
    local host="${S3_ENDPOINT:-https://s3.${S3_REGION}.amazonaws.com}"
    echo "${host%/}/${S3_BUCKET}/${key}"
}

# A credencial entra pelo stdin (-K -), não por argv: --user apareceria no
# ps / /proc/<pid>/cmdline até o curl apagá-lo.
s3_curl() {
    printf 'user = "%s:%s"\n' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" |
        curl -K - --silent --show-error --fail \
            --retry 3 --retry-delay 5 --retry-connrefused \
            --aws-sigv4 "aws:amz:${S3_REGION}:s3" \
            "$@"
}

s3_upload() {
    local file="$1" key="$2"
    local size
    size=$(stat -c %s "$file")

    if [ "$size" -gt "$S3_MAX_PUT_BYTES" ]; then
        echo "[S3] ERRO: $(basename "$file") tem $((size / 1024 / 1024)) MB e passa do teto de 5 GB do PUT simples." >&2
        return 1
    fi

    if s3_curl --upload-file "$file" "$(s3_url "$key")" >/dev/null; then
        echo "[S3] OK: s3://${S3_BUCKET}/${key}"
    else
        echo "[S3] ERRO: falha ao enviar ${key}" >&2
        return 1
    fi
}

s3_download() {
    local key="$1" dest="$2"
    s3_curl --output "$dest" "$(s3_url "$key")"
}

# Lista as keys sob um prefixo (ListObjectsV2 devolve XML; extrai as tags <Key>).
s3_list() {
    local prefix="$1"
    s3_curl "$(s3_url '')?list-type=2&prefix=${prefix}" \
        | tr '<' '\n' | sed -n 's/^Key>//p'
}
