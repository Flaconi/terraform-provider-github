# Flaconi

This document describes the specifics required for maintaining our own GitHub provider until
this PR has been merged: https://github.com/integrations/terraform-provider-github/pull/802
(by https://github.com/n0rad/terraform-provider-github)


## Keep branches up-to-date
```bash

# Update our own code
git checkout main
git pull origin main
git checkout -b updates

# Update integrations/terraform-provider-github
git remote add integrations https://github.com/integrations/terraform-provider-github
git merge -S integrations/main

# Update PR 802
git remote add n0rad https://github.com/n0rad/terraform-provider-github
git merge -S n0rad/master

```

## Build release artifacts

For git tagging, use the same tag name as integrations/terraform-=provier-github has with a `-flaconi` suffix.

When creating a new GitHub release, use the same release name as integrations/terraform-provider-github
is using for its latest. We're simply enhancing it with n0rad's addition and want to follow their
version scheme.

1. Create git tag with `v[0-9]\.[0-9]\.[0-9]-flaconi` (e.g.: `v4.19.0-flaconi`)
2. Create a GitHub Release with name: `v[0-9]\.[0-9]\.[0-9]` (e.g.: `v4.19.0`)
3. Create `release/` directory
   ```bash
   mkdir release
   ```
4. Build Linux artifacts:
   ```bash
   mkdir .cache
   docker run -it --rm --user $(id -u):$(id -g) -v $(pwd)/.cache:/.cache -v $(pwd):/data -v $(pwd)/release:/go/bin -w /data golang make fmt
   docker run -it --rm --user $(id -u):$(id -g) -v $(pwd)/.cache:/.cache -v $(pwd):/data -v $(pwd)/release:/go/bin -w /data golang make build
   mv release/terraform-provider-github release/terraform-provider-github_4.19.0_linux_amd64
   ```
5. Build MacOS artifacts (requires to be on a Mac):
   ```bash
   make fmt
   make build
   cp /go/bin/terraform-provider-github release/terraform-provider-github_4.19.0_darwin_amd64
   ```
6. Pack artifacts for release page
   ```bash
   # Enter release/ directory
   cd release

   # Make executable
   chmod +x *

   # Zip files
   zip terraform-provider-github_4.19.0_linux_amd64.zip terraform-provider-github_4.19.0_linux_amd64
   zip terraform-provider-github_4.19.0_darwin_amd64.zip terraform-provider-github_4.19.0_darwin_amd64

   # Create SHA256SUMS file
   shasum -a 256 terraform-provider-github_4.19.0_linux_amd64.zip > terraform-provider-github_4.19.0_SHA256SUMS
   shasum -a 256 terraform-provider-github_4.19.0_darwin_amd64.zip >> terraform-provider-github_4.19.0_SHA256SUMS

   # Create binary signature of SHA256SUMS file
   # Use the same gpg identity as uploaded in Terraform Registry account (`EB10297E7BD3F3AD`)
   gpg \
     --local-user EB10297E7BD3F3AD \
     --output terraform-provider-github_4.19.0_SHA256SUMS.sig \
     --detach-sign terraform-provider-github_4.19.0_SHA256SUMS
   ```
7. Upload the following files into the GitHub release:
    - terraform-provider-github_4.19.0_linux_amd64.zip
    - terraform-provider-github_4.19.0_darwin_amd64.zip
    - terraform-provider-github_4.19.0_SHA256SUMS
    - terraform-provider-github_4.19.0_SHA256SUMS.sig
8. Update provider at registry: https://registry.terraform.io/publish/provider/github/Flaconi/terraform-provider-github
