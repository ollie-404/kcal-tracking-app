# FoodTrack

A small personal food tracking web app for up to two users.

## What is included

- Go + Chi server-rendered web app
- PostgreSQL via pgx
- Signed cookie session auth
- bcrypt password hashing
- Manual user creation from the command line
- Daily calorie/macro targets per user
- Meal create/edit/delete
- USDA FoodData Central lookup and manual macro override
- Responsive HTML/CSS for desktop and iPhone Safari
- Docker Compose for app + PostgreSQL + Caddy
- Caddy automatic HTTPS reverse proxy
- Nightly backup helper script

## Local development

1. Copy the environment file:

```bash
cp .env.example .env
```

2. Edit `.env` for local use:

```bash
APP_HOST=localhost
SECURE_COOKIES=false
DATABASE_URL=postgres://foodtrack:change-this-postgres-password@db:5432/foodtrack?sslmode=disable
```

3. Start the stack:

```bash
docker compose up --build
```

4. Create your first user in a second terminal:

```bash
docker compose exec app /app/foodtrack create-user \
  --email you@example.com \
  --name "Ollie" \
  --password 'replace-with-a-strong-password'
```

5. Open the app at:

```text
http://localhost
```

For local development, Caddy may not issue a public certificate for `localhost`; using plain HTTP locally is fine. Production should use HTTPS.

## Production deployment on AWS EC2

These instructions assume:

- Ubuntu 24.04 LTS EC2 instance
- Elastic IP attached
- Security group allows inbound TCP 22 from your IP, and TCP 80/443 from the internet
- Cloudflare DNS hosts `crenel.uk`
- You are deploying to `food.crenel.uk`

### 1. Point DNS at the instance

In Cloudflare DNS, create:

```text
Type: A
Name: food
Content: your Elastic IP
Proxy status: DNS only initially
TTL: Auto
```

Keep the record DNS-only until Caddy has obtained its first certificate.

### 2. Install Docker on the instance

SSH to the server, then install Docker Engine and the Compose plugin using Docker's official Ubuntu instructions. The practical command sequence is:

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl gnupg
sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
sudo chmod a+r /etc/apt/keyrings/docker.gpg
printf '%s\n' \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo \"$VERSION_CODENAME\") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo usermod -aG docker "$USER"
```

Log out and back in so group membership applies, then verify:

```bash
docker --version
docker compose version
```

### 3. Copy the app to the server

Option A, if this is in a git repo:

```bash
sudo mkdir -p /opt/foodtrack
sudo chown "$USER":"$USER" /opt/foodtrack
git clone YOUR_REPO_URL /opt/foodtrack
cd /opt/foodtrack
```

Option B, copy from your machine:

```bash
scp -r foodtrack ubuntu@YOUR_ELASTIC_IP:/home/ubuntu/foodtrack
ssh ubuntu@YOUR_ELASTIC_IP
sudo mv /home/ubuntu/foodtrack /opt/foodtrack
sudo chown -R ubuntu:ubuntu /opt/foodtrack
cd /opt/foodtrack
```

### 4. Configure secrets

```bash
cp .env.example .env
openssl rand -hex 32
```

Edit `.env`:

```bash
nano .env
```

Set at least:

```text
APP_HOST=food.crenel.uk
SECURE_COOKIES=true
POSTGRES_PASSWORD=use-a-long-random-password
DATABASE_URL=postgres://foodtrack:the-same-password@db:5432/foodtrack?sslmode=disable
USDA_API_KEY=your-real-api-key
SESSION_SECRET=output-from-openssl-rand-hex-32
```

Make the file readable only by your user:

```bash
chmod 600 .env
```

### 5. Start the app

```bash
docker compose up -d --build
docker compose ps
docker compose logs -f caddy app
```

Once DNS resolves and ports 80/443 are open, Caddy should obtain a certificate and serve:

```text
https://food.crenel.uk
```

### 6. Create user accounts

```bash
docker compose exec app /app/foodtrack create-user \
  --email ollie.marwood@cgi.com \
  --name "Ollie" \
  --password 'use-a-strong-unique-password' \
  --calories 2000 \
  --protein 150 \
  --carbs 220 \
  --fat 70
```

Add a partner account later with the same command and a different email.

### 7. Optional: switch Cloudflare proxy on

After `https://food.crenel.uk` works directly, you can switch the Cloudflare record from DNS-only to proxied. If you do, set Cloudflare SSL/TLS mode to **Full (strict)** so Cloudflare validates the Caddy-managed certificate on your EC2 instance.

### 8. Backups

Install AWS CLI and configure credentials with write access to a private S3 backup bucket. Then set an S3 destination in `/opt/foodtrack/.env`:

```text
S3_URI=s3://your-foodtrack-backups-bucket/postgres
```

Test a backup:

```bash
./scripts/backup.sh
```

Add a nightly cron job:

```bash
crontab -e
```

Add:

```cron
15 2 * * * cd /opt/foodtrack && ./scripts/backup.sh >> /var/log/foodtrack-backup.log 2>&1
```

### 9. Updating the app

If using git:

```bash
cd /opt/foodtrack
git pull
docker compose up -d --build
docker compose logs -f app
```

The app runs migrations automatically on startup.

### 10. Useful maintenance commands

```bash
# See containers
docker compose ps

# See logs
docker compose logs -f app caddy db

# Restart the app only
docker compose restart app

# Open a psql shell
docker compose exec db psql -U "$POSTGRES_USER" "$POSTGRES_DB"

# Stop everything
docker compose down
```

## Notes and limitations

- USDA values are approximate and depend on the selected food match.
- Editing a USDA meal converts it to a manual meal so the stored macros stay explicit.
- No public registration is implemented by design.
- No native iPhone app is required; use Safari and Add to Home Screen if you want an app icon.
- For production, restrict SSH to your known IP in the AWS security group and never expose PostgreSQL publicly.
