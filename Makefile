# SSO lab: Keycloak IdP + two Go apps (foo, bar).
#   make up    - start Keycloak on :8080 (admin/admin)
#   make foo   - run foo on http://foo.localhost:3001
#   make bar   - run bar on http://bar.localhost:3002
#   make api   - run the OAuth2 resource server on http://localhost:4000
#   make m2m   - get a client-credentials token as "reporter" and call the api
#   make down  - stop Keycloak

.PHONY: up down foo bar api m2m

up:
	docker compose up -d

down:
	docker compose down

foo:
	cd app && go run . -name foo -port 3001 -secret foo-secret -scopes orders:read -peer http://bar.localhost:3002

bar:
	cd app && go run . -name bar -port 3002 -secret bar-secret -peer http://foo.localhost:3001

api:
	cd app && go run ./api

# No user involved: the reporter service authenticates as itself.
m2m:
	@TOKEN=$$(curl -q -s -d grant_type=client_credentials -d client_id=reporter \
		-d client_secret=reporter-secret -d scope=orders:read \
		http://localhost:8080/realms/lab/protocol/openid-connect/token | jq -r .access_token) && \
	curl -q -s -H "Authorization: Bearer $$TOKEN" http://localhost:4000/orders | jq .
