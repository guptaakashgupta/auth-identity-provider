# SSO lab: Keycloak IdP + two Go apps (foo, bar).
#   make up    - start Keycloak on :8080 (admin/admin)
#   make foo   - run foo on http://foo.localhost:3001
#   make bar   - run bar on http://bar.localhost:3002
#   make down  - stop Keycloak

.PHONY: up down foo bar

up:
	docker compose up -d

down:
	docker compose down

foo:
	cd app && go run . -name foo -port 3001 -secret foo-secret -peer http://bar.localhost:3002

bar:
	cd app && go run . -name bar -port 3002 -secret bar-secret -peer http://foo.localhost:3001
