# jea9linux test vectors

This directory stores checked-in RISC-V ELF fixtures for the `jea9linux`
personality tests. Normal `go test` loads the ELF files directly and does not
require a cross compiler.

Regenerate fixtures intentionally with:

```sh
./testvectors/jea9linux/build.sh
```

The script uses `zig cc` to build tiny freestanding C programs with raw Linux
RISC-V syscalls.
