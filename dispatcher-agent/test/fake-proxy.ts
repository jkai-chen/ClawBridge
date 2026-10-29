// A fake egress proxy: CONNECT only, like clawbridge-agentd's. It records each CONNECT target, refuses the ones in
// `refuse` with 403, and only ever tunnels to loopback, so a test can never reach the network through it.
// It dials with the connect function captured when this module loads, before the agent's egress fence is installed.
import net from "node:net";
import type { AddressInfo } from "node:net";

const rawConnect = net.Socket.prototype.connect;

export class FakeProxy {
	readonly targets: string[] = [];
	readonly refuse = new Set<string>();
	/** Requests that were not CONNECT (plain proxying), refused with 405. */
	readonly plain: string[] = [];
	private server: net.Server | undefined;
	private readonly sockets = new Set<net.Socket>();
	url = "";

	/** Listens on 127.0.0.1, on `port` or any free one. */
	async start(port = 0): Promise<this> {
		this.server = net.createServer((client) => {
			this.sockets.add(client);
			client.on("close", () => this.sockets.delete(client));
			client.on("error", () => {});
			let buf = Buffer.alloc(0);
			const onData = (d: Buffer) => {
				buf = Buffer.concat([buf, d]);
				const end = buf.indexOf("\r\n\r\n");
				if (end < 0) return;
				client.off("data", onData);
				const [method, target = ""] = buf.subarray(0, end).toString().split("\r\n")[0].split(" ");
				const early = buf.subarray(end + 4);
				if (method !== "CONNECT") {
					this.plain.push(`${method} ${target}`);
					client.end("HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n");
					return;
				}
				this.targets.push(target);
				const i = target.lastIndexOf(":");
				const host = target.slice(0, i);
				if (this.refuse.has(target) || host !== "127.0.0.1") {
					client.end("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n");
					return;
				}
				const upstream = new net.Socket();
				this.sockets.add(upstream);
				upstream.on("close", () => this.sockets.delete(upstream));
				upstream.on("error", () => client.destroy());
				client.on("error", () => upstream.destroy());
				upstream.once("connect", () => {
					client.write("HTTP/1.1 200 Connection Established\r\n\r\n");
					if (early.length > 0) upstream.write(early);
					client.pipe(upstream);
					upstream.pipe(client);
				});
				(rawConnect as (...a: unknown[]) => net.Socket).call(upstream, Number(target.slice(i + 1)), host);
			};
			client.on("data", onData);
		});
		await new Promise<void>((r) => this.server?.listen(port, "127.0.0.1", r));
		this.url = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
		return this;
	}

	/** Closes every open tunnel, so the next request must open a new one (and be seen). */
	async dropTunnels(): Promise<void> {
		for (const s of this.sockets) s.destroy();
		// Let the client side see the close before its next request.
		await new Promise((r) => setTimeout(r, 50));
	}

	async stop(): Promise<void> {
		for (const s of this.sockets) s.destroy();
		await new Promise<void>((r) => this.server?.close(() => r()));
	}
}
