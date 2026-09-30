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
// app's network or the app joins the agent's). A container on several
// networks gets the address on the first by name.
//
// Label values are strings, but check settings are typed (a port is a
// number, a list of expected statuses a list). Each value is therefore read
// as a YAML scalar or flow collection after substitution: "6379" is the
// number 6379, "true" a bool, "[200, 301]" a list, and anything else stays
// text.
//
// # Naming and failure
//
// An instance is named <check>:<container name> unless a name label says
// otherwise, and tagged with the container's tags (container_name, image,
// compose service…), so its metrics sit with the container's own. A
// container whose labels do not make a valid instance — an unknown check, a
// misspelt setting — is logged once and counted in
// ozy.agent.autodiscovery.errors; labels cannot change without a new
// container, so trying again every sync would only repeat the message.
package autodiscovery
