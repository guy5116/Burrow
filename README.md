# Burrow

Burrow is a private chat program for two people. You send text and pictures straight
from your computer to your friend's computer. There is no company in the middle, no
account to create, no phone number, and no server that stores your messages.

Everything you send is end-to-end encrypted: only you and the person you are talking
to can read it.

- [What you need](#what-you-need)
- [Install](#install)
- [Your first message, step by step](#your-first-message-step-by-step)
- [Try it on one computer first](#try-it-on-one-computer-first)
- [Everyday use](#everyday-use)
- [Make sure it is really them](#make-sure-it-is-really-them)
- [Sending pictures](#sending-pictures)
- [Changing the port](#changing-the-port)
- [If something does not work](#if-something-does-not-work)
- [The desktop app](#the-desktop-app)
- [What Burrow protects, and what it does not](#what-burrow-protects-and-what-it-does-not)
- [More options](#more-options)
- [For developers](#for-developers)

## What you need

- A computer running Linux, macOS or Windows.
- A terminal (on Windows: PowerShell or Windows Terminal).
- [Go](https://go.dev/dl/) 1.26 or newer, to build the program.
- A friend who also has Burrow.
- **One of you must be reachable by the other.** The easiest cases:
  - you are both on the same home or office network, or
  - you both use the same VPN (for example Tailscale or WireGuard), or
  - one of you has forwarded port `47337` on their router.

  If none of those fit, see [More options](#more-options) for Tor, which needs no
  network setup at all.

## Install

```
git clone https://github.com/guy5116/burrow.git
cd burrow
make build
```

No `make` on your system (common on Windows)? This does the same:

```
go build ./cmd/burrow
```

This creates a program called `burrow` in the current folder. On Windows it is
`burrow.exe`. You can run it from there as `./burrow`, or move it somewhere on your
`PATH`. The examples below write `burrow`.

## Your first message, step by step

Two people are involved. In this guide **Alice** is the one who can be reached, and
**Bob** connects to her. Decide between you who is who. If you are on the same
network it does not matter.

### Step 1. Both of you: create your identity (once)

```
burrow init
```

You are asked to choose a passphrase and type it twice. Nothing appears while you
type; that is normal. The passphrase protects your identity and contact list on
your disk. **There is no way to recover it**, so pick something you will remember.

You will see your **fingerprint**, a long code in groups of four letters. That is
your identity in Burrow. It is not secret.

### Step 2. Alice: create an invite

```
burrow invite
```

After your passphrase, Burrow prints one long line starting with `burrow1:`. That
line is the invite. It works **once** and expires after **one hour**.

Burrow guesses your network address. If it guesses wrong, or you use a VPN, tell it
which address Bob should connect to:

```
burrow invite --host 192.168.1.20
```

### Step 3. Alice: send the invite to Bob

Send the `burrow1:...` line to Bob over a channel you already trust. **Treat it
like a password**: anyone who gets it within the hour can connect to you as a new
contact.

### Step 4. Alice: start listening

```
burrow listen
```

The chat window opens and waits.

### Step 5. Bob: connect

```
burrow connect
```

Bob pastes the invite when asked and presses Enter, then types his passphrase.
Nothing appears while pasting or typing; that is on purpose, so neither ends up on
screen or in the shell history.

Within a few seconds both of you see that the other has connected. You are now in
each other's contact list.

### Step 6. Say hello

Type a message and press Enter. It shows up on the other side. That is it.

To leave, type `/quit` or press Ctrl+C.

### Step 7. Give your contact a name

Burrow does not send your name to anyone unless you set one, so a new contact first
appears under the first eight characters of their fingerprint, for example
`4pktanvk`. Type `/contacts` to see it, then give it a name you will recognise:

```
/rename 4pktanvk Alice
```

The name is only for you. It is stored on your computer and never sent.

### Next time

You do not need a new invite. You are contacts now.

- Alice runs `burrow listen`.
- Bob runs `burrow connect Alice`, using the name he gave her.

## Try it on one computer first

You can try everything alone before involving a friend, by running Burrow twice on
the same computer. You play both people. Open **two terminal windows**.

Two copies on one computer need two things kept apart:

- **their own folder**, so each has its own identity and contacts, and
- **their own port**, because two programs cannot listen on the same one.

Your normal Burrow is "Alice". The second copy is "Bob" and keeps everything in a
folder called `burrow-test`.

### Terminal 1: Alice

```
burrow init
burrow invite --host 127.0.0.1
burrow listen
```

Skip `burrow init` if you already have an identity. `127.0.0.1` is the address that
always means "this computer". Copy the `burrow1:...` line that `invite` prints.

### Terminal 2: Bob

```
burrow --config burrow-test --data burrow-test init
burrow --config burrow-test --data burrow-test connect --listen :47338
```

Paste the invite when asked, then type Bob's passphrase.

The order matters: `--config` and `--data` go **before** the command, and
`--listen` goes **after** it.

### Chat with yourself

Type in either window and the message appears in the other. Try `/contacts`,
`/safety` and `/image` as well.

### Clean up

When you are done:

1. Type `/quit` in both windows.
2. Delete the `burrow-test` folder. That removes Bob completely.
3. Remove the test contact from your real identity: start `burrow listen`, type
   `/contacts` to see its name, then `/remove` followed by that name.

## Everyday use

Inside the chat, anything you type is sent as a message. Lines that start with `/`
are commands.

| Type this | What happens |
|---|---|
| `/help` | Shows every command |
| `/contacts` | Lists your contacts. `*` means online, `✓` means verified |
| `/to Alice` | Switches the conversation to Alice |
| `/image photo.jpg` | Offers a picture to the current contact |
| `/file backup.zip` | Offers any file to the current contact |
| `/accept 1` or `/reject 1` | Answers a picture someone offered you |
| `/safety Alice` | Shows the safety number for Alice |
| `/verify Alice` | Marks Alice as verified |
| `/rename Alice Ali` | Gives a contact a nickname of your choice |
| `/invite` | Creates an invite without leaving the chat |
| `/quit` | Leaves |

Keys: **Enter** sends, **Tab** switches between contacts, **Page Up** and
**Page Down** scroll.

If the connection drops, your messages wait and are delivered when you are
connected again, as long as you keep Burrow open. Messages that were still waiting
when you quit are not kept.

## Make sure it is really them

The first time you connect, Burrow marks the contact as **UNVERIFIED**. The
connection is already encrypted, but you have not yet confirmed that the person on
the other end is who you think.

To confirm:

1. Both of you type `/safety` followed by the other's name.
2. Each of you sees twelve groups of five digits.
3. Compare them over a phone call or in person. They must match exactly.
4. If they match, both type `/verify` followed by the name.

If the numbers do **not** match, stop. Someone may be in the middle. Remove the
contact and exchange a new invite over a channel you trust more.

## Sending pictures

```
/image holiday.jpg A caption if you like
```

- Before anything is sent, Burrow removes hidden information from the file, such as
  where and when the photo was taken and which camera took it.
- The file name is never sent.
- The other person is asked first. Nothing is downloaded until they type `/accept`.
- Received pictures are saved in Burrow's `images` folder, and the chat shows the
  exact path.
- If the connection drops halfway, send the same picture again and the download
  continues where it stopped.

PNG, JPEG, WebP and GIF are supported, up to 25 MiB.

## Sending other files

```
/file backup.zip A caption if you like
```

Any file works: `.zip`, `.rar`, `.pdf`, documents, anything.

- **The other person sees the size first.** Their chat shows something like
  `offer #1, a file: 1.20 GiB, type .zip`, and nothing is downloaded until they type
  `/accept 1`. `/reject 1` declines. `/transfers` lists the offers that are waiting.
- **The file name is never sent**, only its type. The received file is saved as
  `file-` followed by a short code and the type, for example `file-3fa91c02.zip`. Use
  the caption to say what it is.
- **The content is sent exactly as it is.** Burrow cleans hidden information out of
  pictures, but it cannot do that for other files. A document may still contain its
  author's name, and an archive contains the names of the files inside it.
- A picture sent with `/file` is treated like `/image`, so it is still cleaned.
- Burrow never opens a received file. Only open one yourself if you trust the sender.
- If the connection drops halfway, send the same file again and the download continues
  where it stopped.

### Choosing how large a file you accept

```
burrow config set max_file_mib 500
```

The number is in MiB. The default is 100. Your contact's Burrow learns the limit when
you connect and refuses to send anything larger, so you are not even asked. Set it to
`0` to refuse all files that are not pictures. `max_image_mib` does the same for
pictures. Restart Burrow after changing either.

## Changing the port

Burrow listens on port `47337`. Change it when something else uses that port, when
you run two copies on one computer, or when your router forwards a different one.

**For good.** This is saved in your settings:

```
burrow config set listen_port 47338
```

Check the current value with `burrow config get listen_port`.

**For one run only.** This changes nothing in your settings:

```
burrow listen --listen :47338
burrow connect --listen :47338 Alice
```

**After changing the port, remember:**

- **Make a new invite.** An invite contains the port it was made for, so older
  invites point at the old port.
- **If you used `--listen`, create the invite inside the chat** by typing
  `/invite`. That uses the port you are really listening on. The separate
  `burrow invite` command uses the port saved in your settings.
- **Contacts who connect to you need a new invite.** They saved your old port.
  Contacts that you connect to are not affected.
- **Port forwarding and firewalls** must allow the new port.

Any port between 1024 and 65535 that nothing else uses is fine.

## If something does not work

**"Could not establish a secure session"**
Burrow cannot tell these apart on purpose, so check them in order:

1. Is the other person running `burrow listen` right now?
2. Are you on the same network or VPN? Can you reach their address at all?
3. Was the invite older than one hour, or already used? Ask for a new one.
4. Did the invite contain the right address? Alice can run
   `burrow invite --host <her address>`.

**"address already in use"**
Something on your computer already uses port `47337`, most likely another Burrow
that is still running. Close it, or use another port: see
[Changing the port](#changing-the-port).

**"store in use"**
Burrow is already running somewhere, maybe in another terminal or as the desktop
app. Close it first.

**"wrong passphrase or corrupted store"**
The passphrase was mistyped. Try again.

**"no identity yet; run `burrow init` first"**
Do [Step 1](#step-1-both-of-you-create-your-identity-once).

**A firewall asks whether to allow Burrow**
Allow it on private networks. The side that listens must accept incoming
connections on port `47337`.

**I forgot my passphrase**
It cannot be recovered. Delete Burrow's data folder, run `burrow init` again, and
exchange new invites with your contacts. To them you will be a new person.

| System | Data folder |
|---|---|
| Linux | `~/.local/share/burrow` |
| macOS | `~/Library/Application Support/burrow` |
| Windows | `%APPDATA%\burrow` |

## The desktop app

If you prefer windows and buttons, there is a desktop version with the same
features:

```
make build-gui
./burrow-gui
```

Building it needs a C compiler and the graphics development packages for your
system. On Debian or Ubuntu: `sudo apt install gcc libgl1-mesa-dev xorg-dev`.

The desktop app and the terminal program share the same identity and contacts, but
only one of them can run at a time.

## What Burrow protects, and what it does not

**Protected**

- The content of your messages and pictures, from anyone watching the network,
  including someone who records the traffic today and gets a quantum computer later.
- Against someone pretending to be your contact, once you have verified each other.
- Past conversations, even if a key is stolen later.
- Hidden data inside pictures.
- Your identity and contact list on disk, by your passphrase.
- By default no message history is kept at all.

**Not protected**

- A computer that is already compromised, for example by malware or someone looking
  at your screen.
- What the other person does with what you sent them.
- Your IP address from the person you talk to, when you connect directly. Tor hides
  it; see below.
- The fact that you are using Burrow, and when. Someone watching the network can see
  that two computers talk and roughly how much, but not what is said.
- Anyone who ever had your invite or your fingerprint can tell whether you are
  online at a given address.

The full list is in [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md).

## More options

These are optional. Change them with `burrow config set <name> <value>` and see all
of them with `burrow config get`.

| Setting | What it does | Default |
|---|---|---|
| `transport` | `tcp` connects directly. `tor` hides both IP addresses and needs no port forwarding, but is slower and needs the `tor` program installed. `both` does both | `tcp` |
| `display_name` | A name shown to new contacts | empty |
| `listen_port` | The port others connect to | `47337` |
| `history` | Keep an encrypted copy of your conversations. `burrow burn` deletes it | off |
| `mdns` | Find contacts on the same local network automatically. It reveals that some Burrow user is on the network | off |
| `typing` | Show "is typing…" | off |
| `paranoid_images` | Re-encode every picture so not even the camera model can be guessed | off |
| `max_file_mib` | Largest file you accept or send, in MiB. `0` refuses files | `100` |
| `max_image_mib` | Largest picture you accept or send, in MiB | `25` |
| `auto_accept_from_verified` | Skip the accept question for pictures from verified contacts. Other files always ask | off |

With `transport` set to `tor`, create invites from inside the chat with
`/invite tor`.

Other commands: `burrow id` shows your fingerprint, `burrow contacts list` shows
your contacts, `burrow passphrase` changes your passphrase, and
`burrow --plain --json listen` gives a line-based mode for scripts.

## For developers

```
make build        # CLI, no CGo
make build-gui    # desktop GUI (needs CGo + OpenGL/X11 dev packages)
go build -tags memguard ./cmd/burrow   # optional: identity key in locked memory (docs/SECURITY.md)
make test         # go test -race ./...
make lint         # gofmt, vet, staticcheck, gosec, govulncheck, golangci-lint (run `make tools` once)
make docs-check   # docs/PROTOCOL.md constants match internal/wire
make bench        # benchmarks compared with bench/baseline.json
```

- Design and rules: [CLAUDE.md](CLAUDE.md)
- Wire protocol: [docs/PROTOCOL.md](docs/PROTOCOL.md)
- Security notes and how to report a vulnerability: [docs/SECURITY.md](docs/SECURITY.md)
- Release status: [docs/RELEASE_CHECKLIST.md](docs/RELEASE_CHECKLIST.md)
- Current work: [docs/STATUS.md](docs/STATUS.md)
