#!/bin/bash
cd /opt/AutoSeedRelay
sed -i '/build: ./d' docker-compose.yml
docker compose pull relay
docker compose up -d --force-recreate relay
sleep 8
docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Image}}"
echo "==="
docker logs autoseedrelay 2>&1 | tail -3
