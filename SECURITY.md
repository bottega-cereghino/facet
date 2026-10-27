# Security

facet is maintained by one person. Only the latest release gets fixes.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private reporting instead:
the **Security** tab of this repository, then **Report a vulnerability**.

You should get an answer within a week. A confirmed issue is fixed in a new
release, and the advisory is published once the fix is out.

## What counts

facet runs git, tmux and the GitHub CLI on your machine with your credentials.
Anything that lets crafted input (an issue body, a manifest, a branch name)
run a command, write outside the intended directory, or leak a token is in
scope.
