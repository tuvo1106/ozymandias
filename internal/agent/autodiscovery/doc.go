// Package autodiscovery runs checks for containers that ask for them.
//
// # The idea
//
// A container knows what it is; the agent does not. Rather than keep a
// config file of every Redis in the fleet, and edit it whenever one moves,
// the container carries labels saying which check to run against it and
// with what settings:
//
//	labels:
//	  ozy.check.redis.host: "%%host%%"
//	  ozy.check.redis.port: "%%port%%"
//
// The [Discovery] lists the Docker daemon's running containers every few
// seconds, and for each ozy.check.<check>.<setting> group it builds an
// instance of that check — as if it had been written under
// collectors.checks — and adds it to the collector scheduler. When the
// container goes, so does its instance. This is the same model as
// Datadog's autodiscovery and Prometheus's docker_sd: configuration lives
// with the thing being monitored.
//
// # Template variables and label values
//
// %%host%% becomes the container's IP address and %%port%% its lowest
// exposed port, since neither is known until the container runs. The
// address is only useful if the agent can reach it: the agent and the
// container must share a Docker network (in compose, the agent joins the
// app's network or the app joins the agent's). The address is taken on
// Options.Network, or on the container's only network; a container on
// several with none named is refused, since the agent cannot tell which of
// its addresses it can route to.
//
// Label values are strings, but check settings are typed (a port is a
// number, a list of expected statuses a list). Each value goes to the check
// as a plain YAML scalar, which its config struct decodes: "6379" is the
// number 6379 for an int, "true" a bool, and for a text setting the value
// exactly as written — it is never parsed as YAML, where "0123" would be
// octal and "pa ss #1" would end at a comment. A flow list "[200, 301]" is
// the one value that is parsed.
//
// # Naming and folding
//
// An instance is named <check>:<container name> unless a name label says
// otherwise, and tagged with the container's tags (container_name, image,
// compose service…), so its metrics sit with the container's own. The name
// is the one after container_name_rewrite. Containers the rewrite folds
// into one name have the same tags, so their checks would write the same
// series; each gets a replica:<n> tag, the lowest number no other replica
// of that name holds, so the series number as many as run at once.
//
// # Changes and failure
//
// Every sync resolves each instance's settings again. A container restarted
// in place keeps its id but may have a new address; different settings
// rebuild the instance. A container whose labels do not make a valid
// instance — an unknown check, a misspelt setting, no address yet — is
// logged once and counted in ozy.agent.autodiscovery.errors, and tried
// again only when its settings change (it gets an address), not every sync.
//
// Discovery polls the container list rather than following the event
// stream the Docker collector's watcher reads. A poll is one cheap call
// every ten seconds, and it cannot miss a change the way a dropped stream
// can: whatever happened between two polls, the second sees the result.
// The cost is a check that starts up to one interval late.
package autodiscovery
