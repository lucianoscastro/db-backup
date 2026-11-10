# Estágio 1: UI web + agendador (backupd) e executor T-SQL (sqlrun).
# Binários estáticos, sem CGO — a lógica de backup continua toda nos scripts;
# o Go só orquestra, serve a UI e fala TDS com o SQL Server.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY web/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/backupd . \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/sqlrun ./sqlrun

# Estágio 2: mongodump/mongorestore compilados do fonte upstream.
# O pacote mongodb-tools da distro é gerado com stdlib e x/crypto defasados —
# o Scout apontava 23 CVEs critical/high só nesses binários. Compilando aqui,
# o toolchain e as dependências ficam sob nosso controle, como no sqlrun.
# Sem CGO: perde só Kerberos/GSSAPI (não usamos); SCRAM e TLS são Go puro.
# Tarball conferido por sha256 e x/* em versões fixas: o build é reproduzível
# e um tarball adulterado (ou um x/* novo comprometido) não entra na imagem.
# Ao subir a versão, atualize o hash: curl -sL <url> | sha256sum
FROM golang:1.27-alpine AS tools
ARG MONGO_TOOLS_VERSION=100.17.0
ARG MONGO_TOOLS_SHA256=27d00697a7715443912ce0c76f11e760cfec450a885ac18b499412d4097eeab2
RUN wget -qO /tmp/mongo-tools.tar.gz "https://github.com/mongodb/mongo-tools/archive/refs/tags/${MONGO_TOOLS_VERSION}.tar.gz" \
 && echo "${MONGO_TOOLS_SHA256}  /tmp/mongo-tools.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/mongo-tools.tar.gz \
 && cd "mongo-tools-${MONGO_TOOLS_VERSION}" \
 # o vendor/ do tarball ficaria dessincronizado após o bump; module mode resolve
 && rm -rf vendor \
 && go get golang.org/x/crypto@v0.58.0 golang.org/x/net@v0.62.0 golang.org/x/text@v0.44.0 golang.org/x/sync@v0.25.0 github.com/klauspost/compress@v1.20.4 \
 && go mod tidy \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.VersionStr=${MONGO_TOOLS_VERSION}" -o /out/mongodump ./mongodump/main \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.VersionStr=${MONGO_TOOLS_VERSION}" -o /out/mongorestore ./mongorestore/main

FROM alpine:3.23

# curl 8.x assina SigV4 nativamente (--aws-sigv4), então o upload S3 não
# precisa de aws-cli nem mc.
RUN apk upgrade --no-cache && apk add --no-cache bash curl ca-certificates tzdata

COPY --from=tools /out/mongodump /out/mongorestore /usr/bin/
COPY --from=build /out/backupd /out/sqlrun /usr/local/bin/
COPY scripts/ /usr/local/bin/
RUN chmod +x /usr/local/bin/*.sh /usr/local/bin/drivers/*.sh \
 && mongodump --version | head -1

# Roda sem privilégios; o uid 1000 casa com o dono usual do volume no host.
RUN adduser -D -u 1000 backup && mkdir -p /backups && chown backup /backups
USER backup

VOLUME ["/backups"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
# "serve" = UI web + agendador. Use "daemon" para rodar sem porta exposta.
CMD ["serve"]
