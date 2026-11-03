#!/bin/sh
# Deploys Tideline to Fly.io in one go: creates the app, its volume and an
# API secret, then deploys a single machine.
#
#   scripts/deploy-fly.sh [app-name] [region]
#
# Needs flyctl (https://fly.io/docs/flyctl/install/) and `fly auth login`.
set -eu

APP="${1:-tideline-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')}"
REGION="${2:-lhr}"

if ! command -v fly >/dev/null 2>&1; then
  echo "flyctl is not installed: see https://fly.io/docs/flyctl/install/" >&2
  exit 1
fi
cd "$(dirname "$0")/.."

SECRET="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"

echo "Creating Fly app $APP in $REGION..."
fly launch --copy-config --name "$APP" --region "$REGION" --ha=false --no-deploy --yes
fly secrets set --app "$APP" --stage TIDELINE_API_SECRET="$SECRET"

echo "Deploying..."
fly deploy --app "$APP" --ha=false

cat <<MSG

Tideline is live at https://$APP.fly.dev

Your API secret (sign in with it, then create tokens for each person and device):

    $SECRET

Save it somewhere safe now: it isn't shown again. You can change it later with
    fly secrets set --app $APP TIDELINE_API_SECRET=...
MSG
