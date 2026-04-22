// tr_dump.cpp — see tr_dump.hpp for the design rationale.
//
// All dumper logic is env-var gated (LIBRISCV_DUMP_DIR). When the env
// var is unset, every entry point returns immediately with no I/O.

#include "tr_dump.hpp"

#include <algorithm>
#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <mutex>
#include <sstream>
#include <string>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>
#include <unordered_map>
#include <vector>

#include "machine.hpp"
#include "memory.hpp"

namespace riscv {

namespace {

std::mutex g_dump_mu;

bool dir_writable(const std::string& path) {
    if (path.empty()) return false;
    struct stat st;
    if (stat(path.c_str(), &st) == 0) {
        return S_ISDIR(st.st_mode);
    }
    // Attempt a single-level mkdir. Parent dirs must already exist.
    return mkdir(path.c_str(), 0755) == 0;
}

std::string fmt_hex(uint64_t v, int width = 0) {
    char buf[32];
    if (width > 0)
        std::snprintf(buf, sizeof(buf), "%0*lx", width, (unsigned long)v);
    else
        std::snprintf(buf, sizeof(buf), "%lx", (unsigned long)v);
    return std::string(buf);
}

std::string env_or_empty(const char* name) {
    const char* v = std::getenv(name);
    if (!v) return {};
    return std::string(v);
}

// Locate the function definition for `funcname` inside `src` and return
// the full text (signature + body). Returns empty string when not found
// or when the token appears only as a call site.
std::string extract_c_function(const std::string& src, const std::string& funcname) {
    const std::string needle = funcname + "(";
    size_t pos = 0;
    while ((pos = src.find(needle, pos)) != std::string::npos) {
        // Reject matches that are not whole identifiers (e.g. foo_f_10de).
        if (pos > 0) {
            char prev = src[pos - 1];
            if (std::isalnum((unsigned char)prev) || prev == '_') { ++pos; continue; }
        }

        // Walk from `(` to the matching `)`, tracking paren depth.
        size_t paren = pos + funcname.size();
        int depth = 1;
        size_t i = paren + 1;
        while (i < src.size() && depth > 0) {
            if (src[i] == '(') ++depth;
            else if (src[i] == ')') --depth;
            ++i;
        }
        if (i >= src.size()) { ++pos; continue; }

        // Skip whitespace, optional attributes, and comments to find `{`.
        while (i < src.size() && std::isspace((unsigned char)src[i])) ++i;
        if (i >= src.size() || src[i] != '{') {
            // Not a definition (probably a call site). Keep searching.
            ++pos;
            continue;
        }

        // Walk back to the start of the line containing the signature.
        size_t sig_start = src.rfind('\n', pos);
        sig_start = (sig_start == std::string::npos) ? 0 : sig_start + 1;

        // Walk forward to the matching `}`.
        size_t brace_open = i;
        int bdepth = 1;
        size_t j = brace_open + 1;
        while (j < src.size() && bdepth > 0) {
            if (src[j] == '{') ++bdepth;
            else if (src[j] == '}') { --bdepth; if (bdepth == 0) break; }
            ++j;
        }
        if (j >= src.size()) return {};
        return src.substr(sig_start, j - sig_start + 1);
    }
    return {};
}

// Read one 2- or 4-byte RISC-V instruction at `pc`. Returns the number
// of bytes consumed (2 or 4). `out_raw` is filled with up to 4 low bytes.
// Uses memcpy_out (const-safe) so this can be called from a const
// Memory& — the path used during translation, which runs inside a
// const method on CPU<W>.
template <int W>
int read_one_insn(const Memory<W>& mem, uint64_t pc, uint32_t& out_raw) {
    out_raw = 0;
    uint16_t lo = 0;
    try {
        mem.memcpy_out(&lo, static_cast<address_type<W>>(pc), sizeof(lo));
    } catch (...) {
        return 0;
    }
    out_raw = lo;
    // RVC: bottom 2 bits are not 0b11 → 2 bytes.
    if ((lo & 0x3) != 0x3) return 2;
    // Full 32-bit: read high half.
    uint16_t hi = 0;
    try {
        mem.memcpy_out(&hi, static_cast<address_type<W>>(pc + 2), sizeof(hi));
    } catch (...) {
        return 0;
    }
    out_raw = ((uint32_t)hi << 16) | lo;
    return 4;
}

// Dump raw hex bytes per instruction across [basepc, endpc). No mnemonics —
// the companion GoCPU VizJit dump holds the disassembly; this section keeps
// columns aligned so `diff` between the two files is useful.
template <int W>
void dump_guest_hex(std::ostream& os, const Memory<W>& mem, uint64_t basepc, uint64_t endpc) {
    uint64_t pc = basepc;
    // Cap work in case endpc is a bad estimate.
    int budget = 256;
    int zero_run = 0;
    while (pc < endpc && budget-- > 0) {
        uint32_t raw = 0;
        int n = read_one_insn<W>(mem, pc, raw);
        if (n == 0) {
            os << "  0x" << fmt_hex(pc, 8) << "  <unreadable>\n";
            break;
        }
        // All-zero encodings are "illegal" — a reliable signal of padding
        // past the real block tail. Stop after two consecutive zero words.
        if (raw == 0) {
            if (++zero_run >= 2) break;
        } else {
            zero_run = 0;
        }
        char buf[64];
        if (n == 2) {
            std::snprintf(buf, sizeof(buf), "  0x%08lx  %04x\n",
                (unsigned long)pc, (unsigned)(raw & 0xffff));
        } else {
            std::snprintf(buf, sizeof(buf), "  0x%08lx  %08x\n",
                (unsigned long)pc, (unsigned)raw);
        }
        os << buf;
        pc += n;
    }
}

} // namespace

template <int W>
void dump_bintr_c_source(
    const std::string& shared_library_code,
    const std::vector<TransMapping<W>>& mappings,
    const Memory<W>& mem,
    uint32_t translation_hash)
{
    const char* dir_cstr = std::getenv("LIBRISCV_DUMP_DIR");
    if (!dir_cstr || !*dir_cstr) return;
    std::string dir(dir_cstr);
    if (!dir_writable(dir)) {
        std::fprintf(stderr,
            "libriscv: LIBRISCV_DUMP_DIR=%s is not a writable dir — skipping dump\n",
            dir.c_str());
        return;
    }

    std::string tag = env_or_empty("LIBRISCV_DUMP_TAG");
    if (tag.empty()) tag = fmt_hex(translation_hash, 8);

    std::lock_guard<std::mutex> lk(g_dump_mu);

    // Group mappings by symbol; each unique symbol = one block function.
    std::unordered_map<std::string, uint64_t> sym_basepc;
    sym_basepc.reserve(mappings.size());
    for (auto& m : mappings) {
        uint64_t a = (uint64_t)m.addr;
        auto it = sym_basepc.find(m.symbol);
        if (it == sym_basepc.end()) sym_basepc.emplace(m.symbol, a);
        else if (a < it->second) it->second = a;
    }

    // Estimate endpc per block from the next block's basepc (sorted).
    std::vector<std::pair<uint64_t, std::string>> ordered;
    ordered.reserve(sym_basepc.size());
    for (auto& kv : sym_basepc) ordered.emplace_back(kv.second, kv.first);
    std::sort(ordered.begin(), ordered.end());

    std::vector<std::string> index_lines;
    index_lines.reserve(ordered.size());

    for (size_t i = 0; i < ordered.size(); ++i) {
        uint64_t basepc = ordered[i].first;
        const std::string& sym = ordered[i].second;
        uint64_t endpc = (i + 1 < ordered.size()) ? ordered[i + 1].first
                                                  : basepc + 64;

        std::string basepc_8 = fmt_hex(basepc, 8);
        std::string endpc_8  = fmt_hex(endpc, 8);
        std::string fname = tag + ".libriscv.asm.pc_0x" + basepc_8 + ".asm";
        std::string path  = dir + "/" + fname;

        std::ostringstream os;
        os << "# libriscv bintr dump\n";
        os << "# run tag:    " << tag << "\n";
        os << "# entry PC:   0x" << basepc_8 << "\n";
        os << "# byte range: 0x" << basepc_8 << "..0x" << endpc_8
           << " (" << (endpc - basepc) << " bytes)\n";
        os << "# symbol:     " << sym << "\n";
        os << "# host code:  see \"Host x86-64\" section below\n";
        os << "\n";

        os << "== Guest RISC-V ==\n";
        dump_guest_hex<W>(os, mem, basepc, endpc);
        os << "\n";

        os << "== libriscv bintr C ==\n";
        std::string body = extract_c_function(shared_library_code, sym);
        if (body.empty()) {
            os << "(could not locate function " << sym << " in generated C)\n";
        } else {
            os << body;
            if (body.empty() || body.back() != '\n') os << '\n';
        }

        std::ofstream ofs(path, std::ios::out | std::ios::trunc);
        if (ofs.is_open()) {
            ofs << os.str();
        } else {
            std::fprintf(stderr, "libriscv: failed to open %s for write\n", path.c_str());
        }

        std::ostringstream iline;
        iline << "0x" << basepc_8 << '\t' << sym << '\t' << fname;
        index_lines.emplace_back(iline.str());
    }

    std::string idx_path = dir + "/" + tag + ".libriscv.asm.index.txt";
    std::ofstream idx(idx_path, std::ios::out | std::ios::trunc);
    if (idx.is_open()) {
        for (auto& l : index_lines) idx << l << "\n";
    }
}

namespace {

std::string hex_dump(const void* ptr, size_t len) {
    std::ostringstream os;
    const uint8_t* p = (const uint8_t*)ptr;
    for (size_t i = 0; i < len; i += 16) {
        char head[32];
        std::snprintf(head, sizeof(head), "  %08zx: ", i);
        os << head;
        for (size_t j = 0; j < 16 && (i + j) < len; ++j) {
            char byte[6];
            std::snprintf(byte, sizeof(byte), "%02x ", p[i + j]);
            os << byte;
        }
        os << '\n';
    }
    return os.str();
}

// Pipe hex bytes through `llvm-mc --disassemble` to produce an Intel-
// syntax x86-64 disassembly. `-output-asm-variant=1` selects Intel
// syntax. On tool-missing or any error, falls back to a 16-bytes-per-
// line hex dump so the file is still useful.
std::string disasm_x86_bytes(const void* ptr, size_t len) {
    if (len == 0) return "(empty)\n";

    // Build a stdin string of "0xNN 0xNN ..." tokens.
    std::string hex;
    hex.reserve(len * 5);
    const uint8_t* p = (const uint8_t*)ptr;
    for (size_t i = 0; i < len; ++i) {
        char b[8];
        std::snprintf(b, sizeof(b), "0x%02x ", p[i]);
        hex += b;
        if ((i & 0x1f) == 0x1f) hex += '\n';
    }
    hex += '\n';

    // Write hex to a tmpfile (avoids shell quoting of a potentially huge
    // string). Then run llvm-mc reading that file as stdin.
    char tmpbuf[] = "/tmp/libriscv_dump_XXXXXX";
    int fd = ::mkstemp(tmpbuf);
    if (fd < 0) return hex_dump(ptr, len);
    ssize_t n = ::write(fd, hex.data(), hex.size());
    ::close(fd);
    bool wrote = (n == (ssize_t)hex.size());

    std::string disasm;
    if (wrote) {
        std::string cmd =
            "llvm-mc --disassemble -triple=x86_64 -output-asm-variant=1 < ";
        cmd += tmpbuf;
        cmd += " 2>/dev/null";
        FILE* pf = ::popen(cmd.c_str(), "r");
        if (pf) {
            char line[4096];
            while (std::fgets(line, sizeof(line), pf)) {
                disasm += line;
            }
            ::pclose(pf);
        }
    }
    ::unlink(tmpbuf);

    if (disasm.empty()) {
        std::string out = hex_dump(ptr, len);
        out += "(llvm-mc unavailable — hex dump shown)\n";
        return out;
    }

    // Truncate at the first run of zero-padding disassembly past the
    // last real instruction. TCC emits trailing 00-bytes between a
    // function's final `ret` and the next symbol; llvm-mc decodes 00 00
    // as "add byte ptr [rax], al". Walk the lines: remember the index
    // after the most recent `ret`; if we see >= 4 consecutive
    // `add byte ptr [rax]...` lines, truncate to that remembered index.
    std::vector<std::string> lines;
    {
        std::istringstream in(disasm);
        std::string ln;
        while (std::getline(in, ln)) lines.push_back(std::move(ln));
    }
    auto is_zero_pad = [](const std::string& s) {
        return s.find("add\tbyte ptr [rax], al") != std::string::npos
            || s.find("add byte ptr [rax], al") != std::string::npos;
    };
    auto trimmed_ret_pos = [&]() -> size_t {
        size_t last_ret = 0;
        bool have_ret = false;
        int pad_run = 0;
        for (size_t i = 0; i < lines.size(); ++i) {
            const auto& s = lines[i];
            if (s.find("\tret") != std::string::npos ||
                (s.size() > 0 && s.rfind("ret", 0) == s.size() - 3)) {
                last_ret = i + 1;
                have_ret = true;
                pad_run = 0;
            } else if (is_zero_pad(s)) {
                ++pad_run;
                if (have_ret && pad_run >= 4) return last_ret;
            } else {
                pad_run = 0;
            }
        }
        return lines.size();
    };
    size_t keep = trimmed_ret_pos();

    std::string trimmed;
    trimmed.reserve(disasm.size());
    for (size_t i = 0; i < keep; ++i) {
        trimmed += lines[i];
        trimmed += '\n';
    }
    if (keep < lines.size()) {
        char note[96];
        std::snprintf(note, sizeof(note),
            "(%zu lines of zero-padding after last ret elided)\n",
            lines.size() - keep);
        trimmed += note;
    }
    return trimmed;
}

} // namespace

template <int W>
void dump_bintr_host_asm(
    const Mapping<W>* mappings,
    unsigned nmappings,
    const bintr_block_func<W>* handlers,
    unsigned unique_count,
    uint32_t translation_hash)
{
    const char* dir_cstr = std::getenv("LIBRISCV_DUMP_DIR");
    if (!dir_cstr || !*dir_cstr) return;
    std::string dir(dir_cstr);
    if (!dir_writable(dir)) return;

    std::string tag = env_or_empty("LIBRISCV_DUMP_TAG");
    if (tag.empty()) tag = fmt_hex(translation_hash, 8);

    std::lock_guard<std::mutex> lk(g_dump_mu);

    // For each unique block function, find the smallest PC that maps
    // to it — that's the block's entry point (basepc), which we used
    // as the filename in Phase 1.
    std::vector<uint64_t> basepc_for(unique_count, UINT64_MAX);
    for (unsigned i = 0; i < nmappings; ++i) {
        unsigned idx = mappings[i].mapping_index;
        uint64_t a = (uint64_t)mappings[i].addr;
        if (idx < unique_count && a < basepc_for[idx]) basepc_for[idx] = a;
    }

    // Gather (basepc, func_ptr, unique_index) tuples, filtering out any
    // unique handler that has no matching mapping (shouldn't happen in
    // practice, but be defensive).
    struct Entry { uint64_t basepc; uintptr_t fp; unsigned idx; };
    std::vector<Entry> ents;
    ents.reserve(unique_count);
    for (unsigned i = 0; i < unique_count; ++i) {
        if (basepc_for[i] == UINT64_MAX) continue;
        if (handlers[i] == nullptr) continue;
        ents.push_back({basepc_for[i], (uintptr_t)handlers[i], i});
    }

    // Sort by function pointer to derive per-function byte ranges.
    std::vector<Entry> by_fp = ents;
    std::sort(by_fp.begin(), by_fp.end(),
        [](const Entry& a, const Entry& b){ return a.fp < b.fp; });

    // The `mappings` C array sits in the generated shared library's
    // read-only data, emitted after all block functions. When it lands
    // near the text section (which TCC's in-memory relocation typically
    // arranges), it serves as a conservative upper bound for the last
    // function's tail — clearer than a blind size cap.
    uintptr_t mappings_addr = (uintptr_t)mappings;

    std::unordered_map<unsigned, size_t> idx_to_len;
    for (size_t k = 0; k < by_fp.size(); ++k) {
        uintptr_t end;
        if (k + 1 < by_fp.size()) {
            end = by_fp[k + 1].fp;
        } else if (mappings_addr > by_fp[k].fp
                   && mappings_addr - by_fp[k].fp < (1ull << 20)) {
            end = mappings_addr;
        } else {
            end = by_fp[k].fp + 2048; // conservative fallback cap
        }
        size_t len = (end > by_fp[k].fp) ? (size_t)(end - by_fp[k].fp) : 0;
        // Guard against pathologically large ranges that would pipe a
        // ton of junk through llvm-mc.
        if (len > (1ull << 16)) len = 1ull << 16;
        idx_to_len[by_fp[k].idx] = len;
    }

    // For each block, append the x86-64 section to the per-block file
    // written in Phase 1. Files are addressed by basepc.
    for (auto& e : ents) {
        std::string basepc_8 = fmt_hex(e.basepc, 8);
        std::string fname = tag + ".libriscv.asm.pc_0x" + basepc_8 + ".asm";
        std::string path  = dir + "/" + fname;

        size_t len = idx_to_len[e.idx];
        std::string dis = disasm_x86_bytes((const void*)e.fp, len);

        std::ofstream ofs(path, std::ios::out | std::ios::app);
        if (!ofs.is_open()) continue;
        ofs << "\n";
        ofs << "== Host x86-64 (from TCC) ==\n";
        ofs << "# fp=0x" << fmt_hex((uint64_t)e.fp) << " len=" << len << " bytes\n";
        ofs << dis;
        if (!dis.empty() && dis.back() != '\n') ofs << '\n';
    }
}

// Explicit template instantiations — match tr_translate.cpp's pattern.
#ifdef RISCV_32I
template void dump_bintr_c_source<4>(
    const std::string&, const std::vector<TransMapping<4>>&, const Memory<4>&, uint32_t);
template void dump_bintr_host_asm<4>(
    const Mapping<4>*, unsigned, const bintr_block_func<4>*, unsigned, uint32_t);
#endif
#ifdef RISCV_64I
template void dump_bintr_c_source<8>(
    const std::string&, const std::vector<TransMapping<8>>&, const Memory<8>&, uint32_t);
template void dump_bintr_host_asm<8>(
    const Mapping<8>*, unsigned, const bintr_block_func<8>*, unsigned, uint32_t);
#endif
#ifdef RISCV_128I
template void dump_bintr_c_source<16>(
    const std::string&, const std::vector<TransMapping<16>>&, const Memory<16>&, uint32_t);
template void dump_bintr_host_asm<16>(
    const Mapping<16>*, unsigned, const bintr_block_func<16>*, unsigned, uint32_t);
#endif

} // namespace riscv
