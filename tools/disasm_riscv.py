#!/usr/bin/env python3
"""
Simple RISC-V disassembler for RV64GC ELF binaries.
Handles mixed 16-bit (RVC) and 32-bit instructions correctly.

Usage:
    python3 tools/disasm_riscv.py <elf_file> [start_va] [end_va]
    python3 tools/disasm_riscv.py riscv-elf-tests/rv64ui-p-sraw 0x3c0 0x420
    python3 tools/disasm_riscv.py riscv-elf-tests/rv64ui-p-sraw   # full disassembly
"""

import struct
import sys

# ── ELF parsing ──

def load_elf(path):
    data = open(path, 'rb').read()
    entry = struct.unpack_from('<Q', data, 0x18)[0]
    phoff = struct.unpack_from('<Q', data, 0x20)[0]
    phentsz = struct.unpack_from('<H', data, 0x36)[0]
    phnum = struct.unpack_from('<H', data, 0x38)[0]
    segments = []
    for i in range(phnum):
        off = phoff + i * phentsz
        ptype = struct.unpack_from('<I', data, off)[0]
        if ptype == 1:  # PT_LOAD
            p_offset = struct.unpack_from('<Q', data, off + 8)[0]
            p_vaddr = struct.unpack_from('<Q', data, off + 16)[0]
            p_filesz = struct.unpack_from('<Q', data, off + 32)[0]
            segments.append((p_vaddr, p_offset, p_filesz))
    return data, entry, segments


def va_to_file(segments, va):
    for vaddr, foff, fsz in segments:
        if vaddr <= va < vaddr + fsz:
            return foff + (va - vaddr)
    return None


def code_extent(segments):
    """Return (min_va, max_va) across all loadable segments."""
    lo = min(s[0] for s in segments)
    hi = max(s[0] + s[2] for s in segments)
    return lo, hi


# ── Register names ──

ABI_NAMES = [
    'zero', 'ra', 'sp', 'gp', 'tp', 't0', 't1', 't2',
    's0', 's1', 'a0', 'a1', 'a2', 'a3', 'a4', 'a5',
    'a6', 'a7', 's2', 's3', 's4', 's5', 's6', 's7',
    's8', 's9', 's10', 's11', 't3', 't4', 't5', 't6',
]


def rn(r):
    """Register name: x<N> with ABI alias."""
    return f'x{r}'


# ── 32-bit instruction disassembly ──

def dis32(va, raw):
    opcode = raw & 0x7F
    rd = (raw >> 7) & 0x1F
    funct3 = (raw >> 12) & 0x7
    rs1 = (raw >> 15) & 0x1F
    rs2 = (raw >> 20) & 0x1F
    funct7 = raw >> 25

    if opcode == 0x3B:  # OP-32
        ops = {
            (0x00, 0): 'addw', (0x20, 0): 'subw',
            (0x00, 1): 'sllw', (0x00, 5): 'srlw', (0x20, 5): 'sraw',
            (0x01, 0): 'mulw', (0x01, 4): 'divw', (0x01, 5): 'divuw',
            (0x01, 6): 'remw', (0x01, 7): 'remuw',
        }
        name = ops.get((funct7, funct3), f'op32 f7={funct7:#x} f3={funct3}')
        return f'{name} {rn(rd)},{rn(rs1)},{rn(rs2)}'
    elif opcode == 0x33:  # OP
        ops = {
            (0x00, 0): 'add', (0x20, 0): 'sub', (0x00, 1): 'sll',
            (0x00, 2): 'slt', (0x00, 3): 'sltu', (0x00, 4): 'xor',
            (0x00, 5): 'srl', (0x20, 5): 'sra', (0x00, 6): 'or', (0x00, 7): 'and',
            (0x01, 0): 'mul', (0x01, 1): 'mulh', (0x01, 2): 'mulhsu',
            (0x01, 3): 'mulhu', (0x01, 4): 'div', (0x01, 5): 'divu',
            (0x01, 6): 'rem', (0x01, 7): 'remu',
        }
        name = ops.get((funct7, funct3), f'op f7={funct7:#x} f3={funct3}')
        return f'{name} {rn(rd)},{rn(rs1)},{rn(rs2)}'
    elif opcode == 0x13:  # OP-IMM
        imm = raw >> 20
        if imm & 0x800:
            imm -= 0x1000
        shamt = imm & 0x3f
        if funct3 == 0:
            return f'addi {rn(rd)},{rn(rs1)},{imm}'
        elif funct3 == 1:
            return f'slli {rn(rd)},{rn(rs1)},{shamt}'
        elif funct3 == 2:
            return f'slti {rn(rd)},{rn(rs1)},{imm}'
        elif funct3 == 3:
            return f'sltiu {rn(rd)},{rn(rs1)},{imm}'
        elif funct3 == 4:
            return f'xori {rn(rd)},{rn(rs1)},{imm}'
        elif funct3 == 5:
            if funct7 & 0x20:
                return f'srai {rn(rd)},{rn(rs1)},{shamt}'
            return f'srli {rn(rd)},{rn(rs1)},{shamt}'
        elif funct3 == 6:
            return f'ori {rn(rd)},{rn(rs1)},{imm}'
        elif funct3 == 7:
            return f'andi {rn(rd)},{rn(rs1)},{imm}'
        return f'opi f3={funct3} imm={imm}'
    elif opcode == 0x1B:  # OP-IMM-32
        imm = raw >> 20
        if imm & 0x800:
            imm -= 0x1000
        shamt = imm & 0x1f
        if funct3 == 0:
            return f'addiw {rn(rd)},{rn(rs1)},{imm}'
        elif funct3 == 1:
            return f'slliw {rn(rd)},{rn(rs1)},{shamt}'
        elif funct3 == 5:
            if funct7 & 0x20:
                return f'sraiw {rn(rd)},{rn(rs1)},{shamt}'
            return f'srliw {rn(rd)},{rn(rs1)},{shamt}'
        return f'opi32 f3={funct3}'
    elif opcode == 0x37:  # LUI
        imm = raw & 0xFFFFF000
        if imm & 0x80000000:
            imm -= 0x100000000
        return f'lui {rn(rd)},0x{imm & 0xffffffff:x}'
    elif opcode == 0x17:  # AUIPC
        imm = raw & 0xFFFFF000
        if imm & 0x80000000:
            imm -= 0x100000000
        return f'auipc {rn(rd)},0x{imm & 0xffffffff:x}'
    elif opcode == 0x63:  # BRANCH
        imm = (((raw >> 31) & 1) << 12) | (((raw >> 7) & 1) << 11) | \
              (((raw >> 25) & 0x3f) << 5) | (((raw >> 8) & 0xf) << 1)
        if imm & 0x1000:
            imm -= 0x2000
        target = va + imm
        ops = {0: 'beq', 1: 'bne', 4: 'blt', 5: 'bge', 6: 'bltu', 7: 'bgeu'}
        name = ops.get(funct3, f'br f3={funct3}')
        return f'{name} {rn(rs1)},{rn(rs2)},0x{target:x}'
    elif opcode == 0x6F:  # JAL
        imm = (((raw >> 31) & 1) << 20) | (((raw >> 12) & 0xff) << 12) | \
              (((raw >> 20) & 1) << 11) | (((raw >> 21) & 0x3ff) << 1)
        if imm & 0x100000:
            imm -= 0x200000
        target = va + imm
        return f'jal {rn(rd)},0x{target:x}'
    elif opcode == 0x67:  # JALR
        imm = raw >> 20
        if imm & 0x800:
            imm -= 0x1000
        return f'jalr {rn(rd)},{rn(rs1)},{imm}'
    elif opcode == 0x03:  # LOAD
        imm = raw >> 20
        if imm & 0x800:
            imm -= 0x1000
        ops = {0: 'lb', 1: 'lh', 2: 'lw', 3: 'ld', 4: 'lbu', 5: 'lhu', 6: 'lwu'}
        name = ops.get(funct3, f'load f3={funct3}')
        return f'{name} {rn(rd)},{imm}({rn(rs1)})'
    elif opcode == 0x23:  # STORE
        imm = ((raw >> 25) << 5) | ((raw >> 7) & 0x1F)
        if imm & 0x800:
            imm -= 0x1000
        ops = {0: 'sb', 1: 'sh', 2: 'sw', 3: 'sd'}
        name = ops.get(funct3, f'store f3={funct3}')
        return f'{name} {rn(rs2)},{imm}({rn(rs1)})'
    elif opcode == 0x2F:  # AMO
        funct5 = raw >> 27
        aq = (raw >> 26) & 1
        rl = (raw >> 25) & 1
        w = 'w' if funct3 == 2 else 'd'
        sfx = ''
        if aq:
            sfx += '.aq'
        if rl:
            sfx += '.rl'
        ops = {
            0x00: f'amoadd.{w}', 0x01: f'amoswap.{w}', 0x02: f'lr.{w}',
            0x03: f'sc.{w}', 0x04: f'amoxor.{w}', 0x08: f'amoor.{w}',
            0x0C: f'amoand.{w}', 0x10: f'amomin.{w}', 0x14: f'amomax.{w}',
            0x18: f'amominu.{w}', 0x1C: f'amomaxu.{w}',
        }
        name = ops.get(funct5, f'amo{funct5:#x}.{w}')
        if funct5 == 0x02:  # LR
            return f'{name}{sfx} {rn(rd)},({rn(rs1)})'
        return f'{name}{sfx} {rn(rd)},{rn(rs2)},({rn(rs1)})'
    elif opcode == 0x73:  # SYSTEM
        if raw == 0x00000073:
            return 'ecall'
        if raw == 0x00100073:
            return 'ebreak'
        if raw == 0x30200073:
            return 'mret'
        if raw == 0x10200073:
            return 'sret'
        if raw == 0x10500073:
            return 'wfi'
        csrn = (raw >> 20) & 0xFFF
        csr_names = {
            0x300: 'mstatus', 0x301: 'misa', 0x302: 'medeleg', 0x303: 'mideleg',
            0x304: 'mie', 0x305: 'mtvec', 0x340: 'mscratch', 0x341: 'mepc',
            0x342: 'mcause', 0x343: 'mtval', 0x344: 'mip',
            0x3A0: 'pmpcfg0', 0x3B0: 'pmpaddr0',
            0x105: 'stvec', 0x180: 'satp',
            0xF14: 'mhartid', 0x001: 'fflags', 0x002: 'frm', 0x003: 'fcsr',
            0x744: 'mnstatus',
        }
        csr_name = csr_names.get(csrn, f'0x{csrn:x}')
        if funct3 == 1:
            return f'csrrw {rn(rd)},{csr_name},{rn(rs1)}'
        if funct3 == 2:
            return f'csrrs {rn(rd)},{csr_name},{rn(rs1)}'
        if funct3 == 3:
            return f'csrrc {rn(rd)},{csr_name},{rn(rs1)}'
        if funct3 == 5:
            return f'csrrwi {rn(rd)},{csr_name},{rs1}'
        if funct3 == 6:
            return f'csrrsi {rn(rd)},{csr_name},{rs1}'
        if funct3 == 7:
            return f'csrrci {rn(rd)},{csr_name},{rs1}'
        return f'sys 0x{raw:08x}'
    elif opcode == 0x0F:  # FENCE
        return 'fence'
    return f'??? 0x{raw:08x}'


# ── 16-bit (RVC) instruction disassembly ──

def rvc_simm6(h):
    imm = ((h >> 2) & 0x1f) | (((h >> 12) & 1) << 5)
    if imm & 0x20:
        imm -= 0x40
    return imm


def dis16(va, h):
    op = h & 3
    funct3 = (h >> 13) & 7

    if op == 0:  # Quadrant 0
        rd_ = ((h >> 2) & 7) + 8
        rs1_ = ((h >> 7) & 7) + 8
        if funct3 == 0:  # C.ADDI4SPN
            imm = (((h >> 6) & 1) << 2) | (((h >> 5) & 1) << 3) | \
                  (((h >> 11) & 3) << 4) | (((h >> 7) & 0xf) << 6)
            if imm == 0:
                return 'c.illegal'
            return f'c.addi4spn {rn(rd_)},{imm}'
        if funct3 == 1:  # C.FLD
            off = (((h >> 5) & 3) << 6) | (((h >> 10) & 7) << 3)
            return f'c.fld f{rd_-8},{off}({rn(rs1_)})'
        if funct3 == 2:  # C.LW
            off = (((h >> 6) & 1) << 2) | (((h >> 5) & 1) << 6) | (((h >> 10) & 7) << 3)
            return f'c.lw {rn(rd_)},{off}({rn(rs1_)})'
        if funct3 == 3:  # C.LD
            off = (((h >> 5) & 3) << 6) | (((h >> 10) & 7) << 3)
            return f'c.ld {rn(rd_)},{off}({rn(rs1_)})'
        if funct3 == 5:  # C.FSD
            rs2_ = ((h >> 2) & 7) + 8
            off = (((h >> 5) & 3) << 6) | (((h >> 10) & 7) << 3)
            return f'c.fsd f{rs2_-8},{off}({rn(rs1_)})'
        if funct3 == 6:  # C.SW
            rs2_ = ((h >> 2) & 7) + 8
            off = (((h >> 6) & 1) << 2) | (((h >> 5) & 1) << 6) | (((h >> 10) & 7) << 3)
            return f'c.sw {rn(rs2_)},{off}({rn(rs1_)})'
        if funct3 == 7:  # C.SD
            rs2_ = ((h >> 2) & 7) + 8
            off = (((h >> 5) & 3) << 6) | (((h >> 10) & 7) << 3)
            return f'c.sd {rn(rs2_)},{off}({rn(rs1_)})'
        return f'rvc.q0 0x{h:04x}'

    elif op == 1:  # Quadrant 1
        if funct3 == 0:  # C.ADDI / C.NOP
            rd = (h >> 7) & 0x1f
            imm = rvc_simm6(h)
            if rd == 0:
                return 'c.nop'
            return f'c.addi {rn(rd)},{imm}'
        if funct3 == 1:  # C.ADDIW
            rd = (h >> 7) & 0x1f
            imm = rvc_simm6(h)
            return f'c.addiw {rn(rd)},{imm}'
        if funct3 == 2:  # C.LI
            rd = (h >> 7) & 0x1f
            imm = rvc_simm6(h)
            return f'c.li {rn(rd)},{imm}'
        if funct3 == 3:  # C.LUI / C.ADDI16SP
            rd = (h >> 7) & 0x1f
            if rd == 2:
                imm = (((h >> 2) & 1) << 5) | (((h >> 3) & 3) << 7) | \
                      (((h >> 5) & 1) << 6) | (((h >> 6) & 1) << 4) | \
                      (((h >> 12) & 1) << 9)
                if imm & 0x200:
                    imm -= 0x400
                return f'c.addi16sp {imm}'
            imm = ((h >> 2) & 0x1f) | (((h >> 12) & 1) << 5)
            if imm & 0x20:
                imm -= 0x40
            uimm = imm << 12
            return f'c.lui {rn(rd)},0x{uimm & 0xfffff:x}'
        if funct3 == 4:  # C.MISC-ALU
            f2 = (h >> 10) & 3
            rd_ = ((h >> 7) & 7) + 8
            rs2_ = ((h >> 2) & 7) + 8
            if f2 == 0:
                shamt = ((h >> 2) & 0x1f) | (((h >> 12) & 1) << 5)
                return f'c.srli {rn(rd_)},{shamt}'
            if f2 == 1:
                shamt = ((h >> 2) & 0x1f) | (((h >> 12) & 1) << 5)
                return f'c.srai {rn(rd_)},{shamt}'
            if f2 == 2:
                imm = rvc_simm6(h)
                return f'c.andi {rn(rd_)},{imm}'
            if f2 == 3:
                bit12 = (h >> 12) & 1
                op2 = (h >> 5) & 3
                if bit12 == 0:
                    ops = {0: 'c.sub', 1: 'c.xor', 2: 'c.or', 3: 'c.and'}
                    return f'{ops[op2]} {rn(rd_)},{rn(rs2_)}'
                else:
                    ops = {0: 'c.subw', 1: 'c.addw'}
                    return f'{ops.get(op2, "c.alu?")} {rn(rd_)},{rn(rs2_)}'
        if funct3 == 5:  # C.J
            bits = (h >> 2) & 0x7FF
            imm = (((bits >> 0) & 1) << 5) | (((bits >> 1) & 7) << 1) | \
                  (((bits >> 4) & 1) << 7) | (((bits >> 5) & 1) << 6) | \
                  (((bits >> 6) & 1) << 10) | (((bits >> 7) & 3) << 8) | \
                  (((bits >> 9) & 1) << 4) | (((bits >> 10) & 1) << 11)
            if imm & 0x800:
                imm -= 0x1000
            return f'c.j 0x{va + imm:x}'
        if funct3 == 6:  # C.BEQZ
            rs1_ = ((h >> 7) & 7) + 8
            imm = (((h >> 2) & 1) << 5) | (((h >> 3) & 3) << 1) | \
                  (((h >> 5) & 1) << 7) | (((h >> 6) & 1) << 6) | \
                  (((h >> 10) & 3) << 3) | (((h >> 12) & 1) << 8)
            if imm & 0x100:
                imm -= 0x200
            return f'c.beqz {rn(rs1_)},0x{va + imm:x}'
        if funct3 == 7:  # C.BNEZ
            rs1_ = ((h >> 7) & 7) + 8
            imm = (((h >> 2) & 1) << 5) | (((h >> 3) & 3) << 1) | \
                  (((h >> 5) & 1) << 7) | (((h >> 6) & 1) << 6) | \
                  (((h >> 10) & 3) << 3) | (((h >> 12) & 1) << 8)
            if imm & 0x100:
                imm -= 0x200
            return f'c.bnez {rn(rs1_)},0x{va + imm:x}'
        return f'rvc.q1 0x{h:04x}'

    elif op == 2:  # Quadrant 2
        rd = (h >> 7) & 0x1f
        rs2 = (h >> 2) & 0x1f
        if funct3 == 0:  # C.SLLI
            shamt = ((h >> 2) & 0x1f) | (((h >> 12) & 1) << 5)
            return f'c.slli {rn(rd)},{shamt}'
        if funct3 == 1:  # C.FLDSP
            off = (((h >> 12) & 1) << 5) | (((h >> 5) & 3) << 3) | (((h >> 2) & 7) << 6)
            return f'c.fldsp f{rd},{off}(sp)'
        if funct3 == 2:  # C.LWSP
            off = (((h >> 12) & 1) << 5) | (((h >> 4) & 7) << 2) | (((h >> 2) & 3) << 6)
            return f'c.lwsp {rn(rd)},{off}(sp)'
        if funct3 == 3:  # C.LDSP
            off = (((h >> 12) & 1) << 5) | (((h >> 5) & 3) << 3) | (((h >> 2) & 7) << 6)
            return f'c.ldsp {rn(rd)},{off}(sp)'
        if funct3 == 4:
            bit12 = (h >> 12) & 1
            if bit12 == 0:
                if rs2 == 0:
                    if rd == 0:
                        return 'c.unimp'
                    return f'c.jr {rn(rd)}'
                return f'c.mv {rn(rd)},{rn(rs2)}'
            else:
                if rd == 0 and rs2 == 0:
                    return 'c.ebreak'
                if rs2 == 0:
                    return f'c.jalr {rn(rd)}'
                return f'c.add {rn(rd)},{rn(rs2)}'
        if funct3 == 5:  # C.FSDSP
            off = (((h >> 10) & 7) << 3) | (((h >> 7) & 7) << 6)
            return f'c.fsdsp f{rs2},{off}(sp)'
        if funct3 == 6:  # C.SWSP
            off = (((h >> 9) & 0xf) << 2) | (((h >> 7) & 3) << 6)
            return f'c.swsp {rn(rs2)},{off}(sp)'
        if funct3 == 7:  # C.SDSP
            off = (((h >> 10) & 7) << 3) | (((h >> 7) & 7) << 6)
            return f'c.sdsp {rn(rs2)},{off}(sp)'
        return f'rvc.q2 0x{h:04x}'

    return f'rvc 0x{h:04x}'


# ── Main disassembly driver ──

def disasm_range(data, segments, start, end):
    va = start
    while va < end:
        fo = va_to_file(segments, va)
        if fo is None or fo + 2 > len(data):
            print(f'  0x{va:05x}: (unmapped)')
            va += 2
            continue
        h = struct.unpack_from('<H', data, fo)[0]
        if h & 3 != 3:  # 16-bit RVC
            print(f'  0x{va:05x}: {h:04x}      {dis16(va, h)}')
            va += 2
        else:
            if fo + 4 > len(data):
                print(f'  0x{va:05x}: {h:04x}      (truncated)')
                break
            raw = struct.unpack_from('<I', data, fo)[0]
            print(f'  0x{va:05x}: {raw:08x}  {dis32(va, raw)}')
            va += 4


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)

    path = sys.argv[1]
    data, entry, segments = load_elf(path)

    if len(sys.argv) >= 4:
        start = int(sys.argv[2], 0)
        end = int(sys.argv[3], 0)
    elif len(sys.argv) == 3:
        start = int(sys.argv[2], 0)
        end = start + 0x100
    else:
        start, end = code_extent(segments)

    print(f'Entry: 0x{entry:x}')
    print(f'Segments: {len(segments)}')
    for vaddr, foff, fsz in segments:
        print(f'  LOAD: vaddr=0x{vaddr:x} foff=0x{foff:x} size=0x{fsz:x}')
    print(f'\nDisassembly 0x{start:x} - 0x{end:x}:')
    disasm_range(data, segments, start, end)


if __name__ == '__main__':
    main()
