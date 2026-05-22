# Docker Push to Aliyun

## Description
Build and push ClawManager Docker image to Aliyun Container Registry.

## Important: Check Login First
Before pushing, ALWAYS verify Docker is already logged in:
```bash
cat ~/.docker/config.json | grep -o '"crpi-[^"]*"' | head -1
docker info 2>/dev/null | grep -i "crpi-" || echo "checking registry auth..."
```

If the registry `crpi-p7zet5k9t1r8eu0j.cn-shenzhen.personal.cr.aliyuncs.com` is listed in `~/.docker/config.json`, **NO login is needed** — credentials are already persisted.

## Registry
- **Address**: `crpi-p7zet5k9t1r8eu0j.cn-shenzhen.personal.cr.aliyuncs.com`
- **Repository**: `yangkin/clawmanager`
- **Tag**: `latest` (default)
- **Platform**: `linux/amd64`

## Quick Push
```bash
cd /Users/yangkai/workspace/ClawManager
./push-image.sh [tag]
```

## Manual Steps
```bash
cd /Users/yangkai/workspace/ClawManager
docker build --platform linux/amd64 -t clawmanager:latest -f Dockerfile .
docker tag clawmanager:latest crpi-p7zet5k9t1r8eu0j.cn-shenzhen.personal.cr.aliyuncs.com/yangkin/clawmanager:latest
docker push crpi-p7zet5k9t1r8eu0j.cn-shenzhen.personal.cr.aliyuncs.com/yangkin/clawmanager:latest
```

## From backend directory
```bash
cd /Users/yangkai/workspace/ClawManager/backend
make push-aliyun
```

## Notes
- Docker login credentials are persisted in `~/.docker/config.json` — no re-login required
- The registry is already authenticated in this environment
- Image is built for `linux/amd64` by default
