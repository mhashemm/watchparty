# Deploying the relay

The `deploy-relay` workflow is run by hand on master from the Actions tab. It
sends its commit, signed with HMAC-SHA256, to a [webhook](https://github.com/adnanh/webhook)
on the VPS. The VPS runs `deploy.sh`, which checks that the commit is on `origin/master`,
checks it out on `master` and runs `docker compose -f relay/compose.yaml up -d --build`.
The script's output comes back as the workflow's log.

- **Roll back** by re-running an older run: it deploys that run's commit.
- A commit that doesn't change the relay rebuilds an identical image, so the
  container isn't restarted and nobody is disconnected.
- **The hook's hostname must not be proxied by Cloudflare**, which cuts a request off at
  100s; a deploy waits for the build.

## One-time setup on the VPS

The checkout the relay already runs from must be on `master`, tracking `origin/master`
over HTTPS (the hook runs `git fetch` with no credentials), with no local changes.

```sh
sudo apt install webhook curl

# the secret, shared with GitHub; readable by root only
printf 'RELAY_DEPLOY_SECRET=%s\n' "$(openssl rand -hex 32)" | sudo tee /etc/relay-deploy.env >/dev/null
sudo chmod 600 /etc/relay-deploy.env

# webhook on 127.0.0.1:9001, as you, from the checkout
cd ~/watchparty
sed "s|@REPO@|$PWD|g; s|@USER@|$USER|g" relay/deploy/relay-deploy.service |
  sudo tee /etc/systemd/system/relay-deploy.service >/dev/null
sudo systemctl daemon-reload && sudo systemctl enable --now relay-deploy
```

In nginx, inside the TLS `server` block of a hostname Cloudflare doesn't proxy. Its
certificate can't be a Cloudflare origin certificate, which only Cloudflare trusts:
`certbot --nginx -d <host>` gets one from Let's Encrypt.

```nginx
location /hooks/ {
    proxy_pass http://127.0.0.1:9001;
    proxy_read_timeout 600s;
}
```

On GitHub, the secret and the hook's URL:

```sh
sudo cat /etc/relay-deploy.env                 # on the VPS: copy the value
gh secret set RELAY_DEPLOY_SECRET              # on your machine: paste it at the prompt
gh variable set RELAY_DEPLOY_URL --body https://<host>/hooks/relay
```

`hooks.json` is read when webhook starts: after a change to it, `sudo systemctl restart relay-deploy`.
Its log: `journalctl -u relay-deploy`.
