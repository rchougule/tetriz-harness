# Using Harness Control Plane as a Go library

Use the library when you already have people, teams and sign-in, and you want the control plane in your own Go program. The [README](control-plane.md#using-it-as-a-go-library) gives a summary.

## What you supply

Import these packages:

- `github.com/tetriz-ai/tetriz-harness/server` for the handler.
- `server/store` for the Postgres store.
- `server/blob` for the body store.

You supply a `server.Directory` of people and teams. Your middleware binds each agent call to a person with `server.WithPerson`. `server.New` returns an error if the Store, the Directory or the Blobs is missing.

The handler does not authenticate requests. It trusts each request that it gets. Thus the host must authenticate each request before it sends the request to the handler:

- The handler serves an `AdminService` call to each caller that reaches it.
- An `AgentService` call acts as the person that the host bound.

## Example host

[`examples/go-library-host`](../examples/go-library-host/main.go) is a complete host in one file. It has:

- a fixed `Directory` with one person, `dev-1`, in one team, `platform`;
- the Postgres store from `DATABASE_URL`, and a blob store in `./blobs`;
- a middleware that accepts `AdminService` calls with `ADMIN_TOKEN` and binds the device token `dev-1-device-token` to `dev-1`.

The middleware is the only guard, because the handler authenticates nothing:

```go
host := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(r.URL.Path, server.AdminPathPrefix):
		if auth != admin || admin == "Bearer " {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
	case strings.HasPrefix(r.URL.Path, server.AgentPathPrefix):
		person, ok := devices[auth]
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(server.WithPerson(r.Context(), person))
	}
	h.ServeHTTP(w, r)
})
```

[`server/example_test.go`](../server/example_test.go) has a similar example that the tests compile.

## Try the example host

1. Start the host from the repository root, with `DATABASE_URL` set to an empty Postgres database:

   ```sh
   DATABASE_URL='postgres://...' ADMIN_TOKEN=example-admin-token go run ./examples/go-library-host
   ```

2. In a second terminal, run these commands. They target a rule at the team and sync a temporary home directory:

```sh
admin() { curl -sS -X POST "http://127.0.0.1:8090/harnessctl.v1.AdminService/$1" \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer example-admin-token' -d "$2"; }
ID=$(admin UpsertArtifact '{"ideType":"IDE_TYPE_CLAUDE_CODE","type":"ARTIFACT_TYPE_RULE","slug":"style","name":"Style","inlineContent":"be terse\n"}' | jq -r .artifactId)
admin SetTargets "{\"artifactId\":\"$ID\",\"scope\":\"ARTIFACT_SCOPE_USER_GLOBAL\",\"audience\":{\"teamIds\":[\"platform\"]}}" >/dev/null
HOME="$(mktemp -d)" harness-agent sync --server http://127.0.0.1:8090 --token dev-1-device-token --no-discover
admin GetEnforcementStatus "{\"artifactId\":\"$ID\"}" | jq -r '.statuses[] | "\(.personId) \(.status)"'
```

```text
sync: 1 artifact(s), 1 target(s); in_sync 1, pending 0, failed 0, ended 0; 0 apply failure(s); reported
dev-1 ENFORCEMENT_STATUS_INSTALLED
```

## Notes

- **Ids:** an id is an opaque string with a maximum of `server.MaxIDLen` bytes. Targets and reports keep the id as it is. Thus an id must always identify the same person or team.
- **Directory changes:** if your `Directory` does not list a person, that person is not in any status. This is true even if targets name the person.
- **Directory calls:** the server reads the directory on each grading pass and on each agent call. Answer from memory or from a cache.
- **Tokens:** in a real host, compare tokens in constant time.
- **Reference host:** `cmd/harnessctl-server` is a complete host that reads a JSON directory file.
