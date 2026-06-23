# Releasing the Tipsy AB-config Java SDK

The Java SDK is published to **Maven Central** via the Sonatype **Central
Publisher Portal** (the same mechanism `page.liam:pine` uses). Business
consumers depend on `io.github.lightspeed-intelligence:tipsy-abconfig` from the default Maven Central
repository — no credentials, no custom `<repositories>` block.

The release logic lives entirely in the `release` Maven profile of
`sdk/java/pom.xml` (source jar + javadoc jar + GPG sign + central-publishing
plugin) and is driven on CI by `.github/workflows/java-sdk.yml` on a
`java-sdk/vX.Y.Z` tag push.

> **First release (0.1.0) is already done** — namespace verified, secrets set,
> GPG key published, the publish pipeline proven end-to-end. For a NEW version
> you only do the 4 quick steps below; the one-time setup in "Prerequisites"
> does **not** need to be repeated.

## TL;DR — releasing a new version (after 0.1.0)

Everything one-time is already configured. To ship `X.Y.Z`:

```bash
# 1. bump version everywhere (parent + 4 children share ONE version) and prepend CHANGELOGs
#    edit: sdk/java/pom.xml <version>; each child <parent><version>;
#          sdk/java/tipsy-abconfig/CHANGELOG.md, sdk/java/tipsy-auth/CHANGELOG.md;
#          version snippets in sdk/java/README.md + docs/usage-and-integration.md §4.3
cd sdk/java && mvn clean test            # 2. must be green
git add -A && git commit -m "release(java-sdk): vX.Y.Z" && git push  # 3. PR → merge to main
git checkout main && git pull
git tag java-sdk/vX.Y.Z && git push origin java-sdk/vX.Y.Z           # 4. tag → CI publishes
```

Then watch the `java-sdk` release run; on success the artifacts appear on Maven
Central within minutes (see "Verify" / "Gotchas" below). **Do not reuse a version
number that was already published** — Central versions are immutable.

## Prerequisites (one-time, org/CI setup)

- **Maven Central namespace `io.github.lightspeed-intelligence`** must be verified on
  the Central Portal via the GitHub-org verification path (the company does not own
  the `tipsy.io` domain, so DNS-TXT verification is not used; namespace ownership
  is proven by GitHub organization control of `Lightspeed-Intelligence`).
- Repo secrets (GitHub → Settings → Secrets → Actions):
  - `CENTRAL_USERNAME` / `CENTRAL_TOKEN` — a Central Portal **token** (user
    token pair, not your portal password).
  - `GPG_PRIVATE_KEY` — ASCII-armored private key whose public half is
    published to a keyserver.
  - `GPG_PASSPHRASE` — the key passphrase (empty string if the key has none).
- A GitHub environment named `release` (optional gate/reviewers) is referenced
  by the release job.

## Release steps

1. **Prepare** (one PR)

   - Decide the new version `X.Y.Z` (SemVer).
   - Bump the version in lock-step:
     - `sdk/java/pom.xml` (`<version>`) and every child module's `<parent>`
       version (`tipsy-abconfig-proto`, `tipsy-auth`, `tipsy-abconfig`,
       `example`). All four modules share the parent version.
     - `sdk/java/tipsy-abconfig/CHANGELOG.md` and
       `sdk/java/tipsy-auth/CHANGELOG.md` — prepend a `## [X.Y.Z] - YYYY-MM-DD`
       section.
     - Update the `<version>` in the install snippets of `sdk/java/README.md`
       and `docs/usage-and-integration.md` if you reference an exact version.
   - Local sanity:
     ```bash
     cd sdk/java
     mvn clean test
     # Dry-run the release packaging WITHOUT publishing (no -Prelease deploy):
     mvn -q -Prelease -DskipTests verify   # builds + signs locally (needs a GPG key)
     ```

2. **Open a release PR**

   - PR title: `release(java-sdk): vX.Y.Z`.
   - Diff scope: the POM versions + CHANGELOGs (+ doc version snippets).
   - Get review approval. Merge to `main`.

3. **Tag the release**

   ```bash
   git checkout main && git pull
   git tag java-sdk/vX.Y.Z
   git push origin java-sdk/vX.Y.Z
   ```

   The `java-sdk.yml` `release` job:
   - verifies the tag version matches the parent POM version (fails fast on a
     mistag);
   - runs `mvn deploy -B -DskipTests -Prelease`, which attaches sources +
     javadoc, GPG-signs, and uploads to the Central Portal with
     `autoPublish=true`.

   The `example` module is excluded from the published set via
   `<excludeArtifacts>tipsy-abconfig-example</excludeArtifacts>` on the parent's
   central-publishing plugin (NOT via `skipPublishing` — see Gotchas #1). Three
   artifacts are published per release: `tipsy-abconfig-proto`, `tipsy-auth`,
   `tipsy-abconfig` (the parent POM is published as a `pom`).

4. **Verify**

   - The release run log must show **`Uploaded bundle successfully, deploymentId:
     … Deployment will publish automatically`** and **`has been validated`** — NOT
     just "Created bundle". If you only see "Created bundle" or "Skipping Central
     Release Publishing at user's request", nothing was uploaded (see Gotchas #1).
   - Probe Maven Central main repo directly (no auth needed); 200 = synced:
     ```bash
     curl -sI https://repo1.maven.org/maven2/io/github/lightspeed-intelligence/tipsy-abconfig/X.Y.Z/tipsy-abconfig-X.Y.Z.jar | head -1
     ```
   - Or resolve it: `mvn dependency:get -Dartifact=io.github.lightspeed-intelligence:tipsy-abconfig:X.Y.Z`.
   - Optionally simulate a downstream consumer end-to-end:
     ```bash
     rm -rf ~/.m2/repository/io/github/lightspeed-intelligence   # force a real Central fetch
     (cd test/dev-e2e/clients/java && mvn -q -DskipTests package) # pulls the released jar, builds fat-jar
     ```
   - Create a GitHub Release for the `java-sdk/vX.Y.Z` tag with the CHANGELOG
     excerpt.

## Versioning

- SemVer; tag scheme `java-sdk/vX.Y.Z` (parallels `python-sdk/vX.Y.Z`).
- All four reactor modules share one version (the parent version). Bump them
  together; do not release a single child at a divergent version.

## Gotchas / lessons learned (from the 0.1.0 release)

1. **CI green ≠ published. The `skipPublishing` trap (this bit us once).**
   `central-publishing-maven-plugin` with `extensions=true` aggregates the WHOLE
   reactor and performs the single bundle upload from the **last reactor module**
   (which here is `example`). Putting `<skipPublishing>true</skipPublishing>` on
   the example to "not publish the demo" therefore **suppressed the entire
   upload** — the run went green but Central showed "No Components Found". Fix
   (current state): the example has **no** `skipPublishing`; instead the parent's
   central plugin lists `<excludeArtifacts>tipsy-abconfig-example</excludeArtifacts>`.
   **Always confirm the log shows "Uploaded bundle … deploymentId" before
   trusting a green run.**

2. **`excludeArtifacts` matches the bare `artifactId`, not a GAV.**
   The plugin (0.7.0) matches each entry against `Artifact.getArtifactId()`. Use
   `tipsy-abconfig-example`, NOT `io.github.lightspeed-intelligence:tipsy-abconfig-example`
   and NOT a 3-part `groupId:artifactId:version` (those silently match nothing).

3. **Sync delay is real.** After a successful upload + validate + auto-publish,
   the artifacts take minutes to ~1h to appear on `repo1.maven.org`. A 404 right
   after a green run is normal — poll the `curl -sI` probe above, don't assume
   failure.

4. **Reusing a version.** If a run fails BEFORE the bundle is published (e.g. the
   skipPublishing trap, or a validation error), the version was never actually on
   Central, so it is safe to fix and re-release the SAME version: move the tag
   (`git tag -d java-sdk/vX.Y.Z && git tag java-sdk/vX.Y.Z <fix-commit> &&
   git push --force origin java-sdk/vX.Y.Z`). Once a version IS published to
   Central it is immutable — you must bump to the next version.

5. **GPG key.** The signing key (`yu <1750462583@qq.com>`, fingerprint
   `E61750ED97B70AFA28A0E2FE0058065D5F0E58F0`) is published to
   `keyserver.ubuntu.com` + `keys.openpgp.org`. It lives in CI secrets
   (`GPG_PRIVATE_KEY` / `GPG_PASSPHRASE`); no local key is needed for a CI
   release. The local dry-run in step 1 (`mvn -Prelease verify`) needs a GPG key
   only if you don't pass `-Dgpg.skip=true`.

6. **Namespace.** `io.github.lightspeed-intelligence` is verified on the Central
   Portal via GitHub-org control of `Lightspeed-Intelligence` (the temporary
   verification repo can be deleted after it shows VERIFIED). It does not expire.

## Notes

- No `distributionManagement` is needed — `central-publishing-maven-plugin`
  resolves the upload endpoint from `publishingServerId=central` mapped to the
  `<server id="central">` in `~/.m2/settings.xml` (injected by `setup-java`).
- If a deployment uploaded but is stuck (validated but not published, or a
  validation error), manage it in the Central Portal → Deployments UI (drop /
  re-publish), then re-run the tag job. See Gotchas #1/#4 for the
  green-but-nothing-published case.
