// tr_dump.hpp — diagnostic dumper for libriscv's binary translation pipeline.
//
// When enabled via the LIBRISCV_DUMP_DIR environment variable, the dumper
// writes one file per compiled block into that directory, mirroring GoCPU's
// VizJit dump format so side-by-side diffs of the two emulators' per-block
// output become feasible.
//
// Filename:  <tag>.libriscv.asm.pc_0x<basepc:08x>.asm
//   <tag> from LIBRISCV_DUMP_TAG if set, else translation_hash as 8 hex.
//
// File sections mirror GoCPU VizJit:
//   # <header>
//   == Guest RISC-V ==        (raw hex per instruction, no disasm)
//   == libriscv bintr C ==    (extracted function body from generated C)
//   == Host x86-64 (from TCC) ==  (phase 2: disassembly of compiled bytes)
//
// Zero overhead when LIBRISCV_DUMP_DIR is unset.

#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "types.hpp"

namespace riscv {

template <int W> struct Memory;

// Mirror of the in-generated-C `struct Mapping` — one per instruction
// PC that enters the translated segment. `mapping_index` is the index
// into the per-block `handlers[]` (unique function pointer) array.
// Same layout as the struct tr_translate.cpp used to define locally;
// centralized here so tr_dump.cpp can accept typed pointers.
template <int W>
struct Mapping {
    address_type<W> addr;
    unsigned mapping_index;
};

// Phase 1: Write header + Guest RISC-V + libriscv C for each compiled
// block. Called just before libtcc_compile, while the full C source is
// still in-scope.
template <int W>
void dump_bintr_c_source(
    const std::string& shared_library_code,
    const std::vector<TransMapping<W>>& mappings,
    const Memory<W>& mem,
    uint32_t translation_hash);

// Phase 2: Append the x86-64 disassembly section to each per-block file,
// using the TCC-compiled function pointers to compute byte ranges.
// Called after activate_dylib has resolved the symbol pointers.
//
// `mappings[0..nmappings)` — flat (addr, index) pairs for every
//     PC-inside-a-translated-block.
// `handlers[0..unique_count)` — one function pointer per unique block.
template <int W>
void dump_bintr_host_asm(
    const Mapping<W>* mappings,
    unsigned nmappings,
    const bintr_block_func<W>* handlers,
    unsigned unique_count,
    uint32_t translation_hash);

} // namespace riscv
