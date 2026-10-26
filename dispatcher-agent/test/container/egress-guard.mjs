// Preload for the container test: records and refuses every TCP connect and DNS lookup that isn't loopback, and
// prints what it saw on exit. Loaded with `node --import`, never through NODE_OPTIONS (the agent refuses that).
import dns from "node:dns";
import net from "node:net";

const LOOPBACK = new Set(["127.0.0.1", "::1", "localhost"]);
const attempts = [];
const connect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function (...args) {
	const first = Array.isArray(args[0]) ? args[0][0] : args[0];
	const o = typeof first === "object" && first !== null ? first : { port: first, host: args[1] };
	if (o.path === undefined && !LOOPBACK.has(o.host ?? "localhost")) {
		attempts.push(`connect ${o.host}:${o.port}`);
		throw new Error("egress refused");
	}
	return connect.apply(this, args);
};
const lookup = dns.lookup;
dns.lookup = function (host, ...rest) {
	if (LOOPBACK.has(host)) return lookup.call(this, host, ...rest);
	attempts.push(`dns ${host}`);
	const callback = rest.at(-1);
	process.nextTick(() => callback(Object.assign(new Error("egress refused"), { code: "ENOTFOUND" })));
};
process.on("exit", () => process.stderr.write(`${JSON.stringify({ egress_attempts: attempts })}\n`));
