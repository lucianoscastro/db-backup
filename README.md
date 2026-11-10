# db-backup

Imagem Docker que faz backup periódico de **MongoDB** e **SQL Server** (arquitetura pronta para outros bancos), com **UI web protegida por login** para listar servidores, listar backups, disparar backup e restaurar. Cada backup pode ser enviado para o **S3**.

**Imagem:** [lucianoscastro/db-backup](https://hub.docker.com/r/lucianoscastro/db-backup) no Docker Hub · **Código-fonte:** [github.com/lucianoscastro/db-backup](https://github.com/lucianoscastro/db-backup)

- **Drivers por engine**: `scripts/drivers/<engine>.sh` — um script novo = um banco novo, sem tocar no resto
- **Um diretório por base**: `/backups/<engine>/<base>/<base>_<timestamp>.<ext>`
- **Agendamento próprio**: roda a cada `INTERVAL_HOURS`, sem cron no host
- **UI web opcional**: login com rate-limit, sessão assinada, um job por vez
- **S3 opcional**: `S3_ENABLED=false` mantém só o backup local
- **Restore** pelo UI, pelo arquivo local ou direto do S3

## Uso

```bash
cp .env.example .env    # configure os engines, o S3 e a ADMIN_PASSWORD

docker run -d --name db-backup \
  --restart unless-stopped \
  --env-file .env \
  -p 8080:8080 \
  -v ./backups:/backups \
  lucianoscastro/db-backup:latest
```

A UI fica em `http://localhost:8080` (usuário `admin` por padrão). Para rodar **sem UI** (sem porta exposta), use o comando `daemon`:

```bash
docker run -d ... lucianoscastro/db-backup:latest daemon
```

Com `docker-compose.yml` (exemplo com SQL Server no mesmo host):

```yaml
services:
  db-backup:
    image: lucianoscastro/db-backup:latest
    container_name: db-backup
    restart: unless-stopped
    env_file: .env
    ports:
      - "8080:8080"
    volumes:
      - ./backups:/backups
      - mssql-backup:/mssql-backup   # mesmo volume do SQL Server (ver abaixo)
    group_add:
      - "10001"                      # gid do mssql: permite ler os .bak (modo 640)
    # Hardening (opcional, mas recomendado): rootfs imutável e sem escalação.
    # O staging do mongodump usa /tmp — com read_only ele vira tmpfs (RAM) e
    # conta no limite de memória. Para bases grandes, troque o tmpfs por um
    # diretório de disco (ex.: ./tmp:/tmp) e ajuste os limites.
    read_only: true
    tmpfs:
      - /tmp:size=128m
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    deploy:
      resources:
        limits:
          memory: 256m
          cpus: "0.5"

  mssql:
    image: mcr.microsoft.com/mssql/server:2022-latest
    environment:
      ACCEPT_EULA: "Y"
      MSSQL_SA_PASSWORD: "${MSSQL_PASSWORD}"
    volumes:
      - mssql-data:/var/opt/mssql
      - mssql-backup:/var/opt/mssql/backup

volumes:
  mssql-data:
  mssql-backup:
```

## Engines

Um engine é habilitado quando suas variáveis obrigatórias estão definidas — configure um, outro ou os dois.

### MongoDB

| Variável | Padrão | Descrição |
|---|---|---|
| `MONGO_URI` | — | Habilita o engine. Todas as bases do cluster são incluídas |
| `EXCLUDE_DBS` | `admin config local` | Bases ignoradas |
| `DUMP_TIMEOUT_MINUTES` | `60` | Aborta um dump travado. **Aumente se as bases forem grandes** |

### SQL Server

O `BACKUP DATABASE` do SQL Server é **sempre server-side**: o `.bak` nasce no disco do servidor. Por isso o diretório de backup do SQL Server precisa ser um volume montado **também neste container** (como no compose acima). O driver manda o servidor gravar o `.bak` lá, move o arquivo para `/backups` e segue o fluxo normal (S3 + retenção).

| Variável | Padrão | Descrição |
|---|---|---|
| `MSSQL_HOST` / `MSSQL_PASSWORD` | — | Habilitam o engine |
| `MSSQL_PORT` | `1433` | |
| `MSSQL_USER` | `sa` | Precisa de permissão de BACKUP/RESTORE |
| `MSSQL_DATABASES` | *(vazio)* | Lista separada por espaço; vazio = todas as bases de usuário |
| `MSSQL_EXCLUDE_DBS` | *(vazio)* | Bases ignoradas |
| `MSSQL_DIR_SERVER` | `/var/opt/mssql/backup` | Diretório de backup **como o SQL Server o vê** |
| `MSSQL_DIR_LOCAL` | `/mssql-backup` | O **mesmo volume**, montado neste container |
| `MSSQL_COMPRESS` | `false` | `WITH COMPRESSION` — exige edição com suporte (não Express) |
| `MSSQL_TIMEOUT_MINUTES` | `60` | Timeout de cada BACKUP/RESTORE |
| `MSSQL_CA_FILE` | — | CA (PEM) que assina o certificado do servidor. Definida, a conexão é cifrada e o certificado validado; vazia, o certificado é aceito sem validação (vulnerável a MITM na rede) |

**Permissões do volume compartilhado** (uma vez, após criar os containers): o SQL Server roda como uid 10001 e cria os `.bak` com modo 640; este container roda como uid 1000.

```bash
# o diretório precisa aceitar escrita dos dois lados
docker exec -u root <container-mssql> chmod 0777 /var/opt/mssql/backup
# e este container precisa do grupo do mssql para ler os .bak (group_add acima,
# ou --group-add 10001 no docker run)
```

> Restore assume o mesmo servidor (ou mesmo layout de disco): usa `WITH REPLACE`, sem `MOVE`.

### Bancos futuros

Um driver novo é um script em `scripts/drivers/<engine>.sh` implementando 4 subcomandos: `enabled`, `info`, `backup`, `restore <db> <arquivo>`. O agendador, a UI, o S3 e a retenção já funcionam sem mudança — o engine aparece na interface automaticamente.

## UI web

- **Login obrigatório**: `ADMIN_PASSWORD` (mín. 8 caracteres) precisa estar definida no modo `serve`; `ADMIN_USER` padrão `admin`
- **Proteções**: comparação em tempo constante, 5 tentativas erradas = 15 min de bloqueio por IP, cookie `HttpOnly`+`SameSite=Strict` assinado (HMAC), verificação de `Origin` nos POSTs, CSP estrito
- **Exposição**: o container serve HTTP puro — na internet, coloque atrás de um reverse proxy com HTTPS (o cookie `Secure` liga sozinho via `X-Forwarded-Proto`)
- **Sessões**: defina `SESSION_SECRET` para o login sobreviver a restarts do container. O logout e a troca de `ADMIN_PASSWORD` invalidam todas as sessões abertas
- **Jobs**: um por vez (backup agendado, backup manual e restore não concorrem); log de cada job visível na UI

| Variável | Padrão | Descrição |
|---|---|---|
| `ADMIN_PASSWORD` | — | **Obrigatória no modo serve** |
| `ADMIN_USER` | `admin` | |
| `WEB_PORT` | `8080` | |
| `SESSION_TTL_HOURS` | `12` | Validade do login |
| `SESSION_SECRET` | *(aleatório)* | Fixe para manter sessões entre restarts (mín. 32 caracteres: `openssl rand -hex 32`) |
| `COOKIE_SECURE` | `auto` | `auto` liga o cookie Secure atrás de HTTPS |
| `PUBLIC_URL` | — | Domínio público, quando o reverse proxy não repassa o Host original (ver abaixo) |

### "origem inválida" atrás de reverse proxy

A UI compara o header `Origin` que o navegador manda com o `Host` que o container recebe, como proteção CSRF. Se o seu reverse proxy (Cloudflare, nginx, Traefik...) não repassar o `Host` original intacto para o container — por exemplo, nginx sem `proxy_set_header Host $host;` —, `Origin` e `Host` não batem e o login falha com **"origem inválida"**.

O jeito certo de resolver é configurar o proxy para repassar o `Host` original. Se isso não for possível, defina `PUBLIC_URL` com o domínio público (ex.: `PUBLIC_URL=https://backup.exemplo.com`): é uma variável do ambiente do container, definida pelo admin, não um header da requisição — por isso não reabre brecha nenhuma de CSRF (ao contrário de confiar em `X-Forwarded-Host`, que um `fetch()` de outra origem poderia forjar).

## Agendamento, retenção e S3

| Variável | Padrão | Descrição |
|---|---|---|
| `INTERVAL_HOURS` | `6` | Intervalo entre os backups. Aceita fração (`0.5` = 30 min) |
| `MAX_BACKUPS` | `3` | Arquivos mantidos **por base** no disco local |
| `RETRY_MINUTES` | `15` | Espera antes de tentar de novo, se o ciclo falhar |
| `RUN_ON_START` | `true` | Faz um backup ao subir o container |
| `BACKUP_DIR` | `/backups` | Onde os arquivos são gravados (monte um volume aqui) |
| `S3_ENABLED` | `false` | Liga o envio offsite |
| `S3_BUCKET` | — | Bucket de destino |
| `S3_PREFIX` | *(vazio)* | Keys: `<prefix>/<engine>/<base>/<arquivo>` |
| `S3_REGION` | `us-east-1` | Região |
| `S3_ENDPOINT` | — | Só para S3-compatíveis (MinIO, R2). Vazio = AWS |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | — | Credencial do upload |

## Comandos (CLI, sem UI)

Prefixe com `docker run --rm --env-file .env -v ./backups:/backups lucianoscastro/db-backup:latest`:

```bash
backup                     # backup de todos os engines habilitados
backup mongo               # só um engine
list                       # lista os backups locais
restore mongo loja         # restaura do arquivo local mais recente
restore mongo loja --from-s3
restore mongo loja --drop  # (mongo) apaga as coleções antes de restaurar
restore mssql vendas       # SQL Server: sempre WITH REPLACE
```

## Segurança: a credencial do backup não deveria conseguir restaurar

A recomendação é dar ao container uma credencial IAM **só com `s3:PutObject`** no prefixo:

```json
{
  "Effect": "Allow",
  "Action": "s3:PutObject",
  "Resource": "arn:aws:s3:::MEU-BUCKET/*"
}
```

Quem invadir o servidor não consegue **ler nem apagar** os backups — só escrever por cima de keys novas. A contrapartida é que `restore --from-s3` **não funciona com essa credencial**: o restore precisa de outra, com `GetObject`/`ListBucket`, usada só na hora de restaurar.

Com `PutObject` apenas, a retenção remota é responsabilidade da **lifecycle policy do bucket** — o container é incapaz de apagar objetos.

## Como funciona

- O **mongodump** roda uma vez por ciclo sem `--db`, o que já descobre e dumpa todas as bases; cada base sai como um `.tar` próprio. A imagem não precisa do `mongosh` (Node.js, ~200 MB) nem do `aws-cli` — o upload é `curl --aws-sigv4`.
- O `mongodump`/`mongorestore` são **compilados do fonte upstream** ([mongo-tools](https://github.com/mongodb/mongo-tools) 100.17.0) no build da imagem, com toolchain Go e `x/crypto`/`x/net` atuais — o pacote pré-compilado da distro carregava dezenas de CVEs de dependências defasadas.
- O SQL Server é acessado pelo **sqlrun**, um executor T-SQL mínimo do próprio projeto (`web/sqlrun`, ~150 linhas sobre o driver oficial [go-mssqldb](https://github.com/microsoft/go-mssqldb)). Ele substitui o go-sqlcmd da Microsoft, que embutia cliente Docker, SSH e SDK Azure — dezenas de CVEs de código que nunca seria executado aqui. O `BACKUP DATABASE ... WITH INIT, FORMAT, CHECKSUM` gera o `.bak` no volume compartilhado e o driver o move para `/backups`.
- A **UI web** é um binário Go (stdlib, sem dependências) que só executa os mesmos scripts da CLI — a lógica de backup vive nos scripts, sempre.

Limites conhecidos:

- O upload usa PUT simples: **teto de 5 GB por arquivo**. Acima disso o backup local continua, mas o envio falha com erro explícito (seria preciso multipart).
- O dump do Mongo vai para um diretório temporário antes de virar `.tar`, então o pico de disco é ~2× o tamanho do dump comprimido.
- A retenção local só apaga o arquivo antigo **depois** que o novo foi gravado e validado.
- Os backups são gravados com permissão `0600` (só o uid 1000 lê) e **sem criptografia**: no S3, ative a criptografia padrão do bucket (SSE).

### Migrando da imagem mongo-only

O layout local mudou de `/backups/<base>/` para `/backups/<engine>/<base>/`:

```bash
mkdir -p backups/mongo && mv backups/<base> backups/mongo/
```

No S3 nada muda se você usava o `S3_PREFIX=mongo` padrão antigo — o prefixo agora é vazio e o engine (`mongo/`) entra no lugar. A CLI de restore ganhou o engine como primeiro argumento (`restore mongo loja` em vez de `restore loja`).
