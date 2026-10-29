# Sign-in: the user's own steps

These are the user's steps, in their own terminal, after you exit. Never run
them yourself: trusting a certificate changes this machine's trust store, and
the sign-in code is a secret. `clawbridge onboard` prints them too, with this
machine's full paths.

Run them from the directory that holds the `clawbridge` checkout.

1. **lan shape only: trust the server's certificate.**

   ```sh
   sh clawbridge/deploy/local-host.sh trust
   ```

   It exports the server's root certificate and prints the commands that trust
   it on this OS. Run those. Each teammate's machine needs the same root.
   If the host name doesn't resolve on a computer, point it at the server's
   LAN address: in the router's DNS, or with a hosts-file entry such as
   `192.0.2.10 clawbridge.lan`.
2. **Make the owner's sign-in code.**

   ```sh
   sh clawbridge/deploy/local-host.sh invite-owner --workspace my-team
   ```

   It refuses without a terminal, and shows the one-time code only there.
3. **Join with that code.**

   ```sh
   clawbridge join --server http://127.0.0.1:8443 --email owner@example.test
   clawbridge join --server https://clawbridge.lan --email owner@example.test
   ```

   Use the first in the solo shape and the second, with your host, in the lan
   shape. It asks for the code without showing it.
4. **Finish onboarding.** Run `clawbridge onboard` again: it goes on to the
   first project and opens ClawBridge. Then
   `clawbridge cloud check workspace` passes.
