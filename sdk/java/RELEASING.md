# Releasing the Tipsy AB-config Java SDK

The Java SDK is published to **Maven Central** via the Sonatype **Central
Publisher Portal** (the same mechanism `page.liam:pine` uses). Business
consumers depend on `io.tipsy:tipsy-abconfig` from the default Maven Central
repository — no credentials, no custom `<repositories>` block.

The release logic lives entirely in the `release` Maven profile of
`sdk/java/pom.xml` (source jar + javadoc jar + GPG sign + central-publishing
plugin) and is driven on CI by `.github/workflows/java-sdk.yml` on a
`java-sdk/vX.Y.Z` tag push.

## Prerequisites (one-time, org/CI setup)

- **Maven Central namespace `io.tipsy`** must be verified on the Central
  Portal (DNS TXT record on `tipsy.io`, or the GitHub-org verification path).
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

   The `example` module is excluded from publishing
   (`skipPublishing=true` in `sdk/java/example/pom.xml`). Three artifacts are
   published per release: `tipsy-abconfig-proto`, `tipsy-auth`,
   `tipsy-abconfig` (the parent POM is published as a `pom`).

4. **Verify**

   - Central Portal → Deployments shows the bundle as PUBLISHED.
   - After Central → Maven Central sync (minutes to ~1h), the coordinates
     resolve:
     ```bash
     mvn dependency:get -Dartifact=io.tipsy:tipsy-abconfig:X.Y.Z
     ```
   - Create a GitHub Release for the `java-sdk/vX.Y.Z` tag with the CHANGELOG
     excerpt.

## Versioning

- SemVer; tag scheme `java-sdk/vX.Y.Z` (parallels `python-sdk/vX.Y.Z`).
- All four reactor modules share one version (the parent version). Bump them
  together; do not release a single child at a divergent version.

## Notes

- No `distributionManagement` is needed — `central-publishing-maven-plugin`
  resolves the upload endpoint from `publishingServerId=central` mapped to the
  `<server id="central">` in `~/.m2/settings.xml` (injected by `setup-java`).
- If a release fails after upload but before publish, drop the deployment in
  the Central Portal UI and re-run the tag job.
