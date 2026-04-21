local ffi = require("ffi")

-- 1. Fibonacci (ALU + Branch)
local function fib(n)
    local a, b = 0ULL, 1ULL -- Use ULL for 64-bit unsigned integers
    for i = 0, n - 1 do
        local t = a + b
        a = b
        b = t
    end
    return a
end

-- 2. Memstress (Memory Throughput)
-- We use ffi.new to create a C-style array
local MEMSTRESS_ELEMS = 4096
local MEMSTRESS_ITERS = 256

local function memstress(buf, n, iters)
    local sum = 0ULL
    for iter = 0, iters - 1 do
        -- Store
        for i = 0, n - 1 do
            buf[i] = bit.bxor(i, iter)
        end
        -- Load + XOR sum
        for i = 0, n - 1 do
            sum = bit.bxor(sum, buf[i])
        end
    end
    return sum
end

-- 3. Sieve of Eratosthenes (ICache/Branchy)
local SIEVE_LIMIT = 1000000

local function sieve(buf, limit)
    -- Fill buffer
    for i = 0, limit do buf[i] = 1 end
    
    -- Sieve logic
    for i = 2, math.sqrt(limit) do
        if buf[i] == 1 then
            for j = i*i, limit, i do
                buf[j] = 0
            end
        end
    end
    
    -- Count primes
    local count = 0
    for i = 2, limit do
        count = count + buf[i]
    end
    return count
end

-- Execution & Timing
local function run_bench()
    -- Allocate buffers via FFI for native performance
    local membuf = ffi.new("uint64_t[?]", MEMSTRESS_ELEMS)
    local sievebuf = ffi.new("uint8_t[?]", SIEVE_LIMIT + 1)

    print("Starting LuaJIT benchmarks...")

    local start = os.clock()
    
    -- Run Fib
    local f_res = fib(500000000)
    
    -- Run Memstress
    local ms_res = memstress(membuf, MEMSTRESS_ELEMS, MEMSTRESS_ITERS)
    
    -- Run Sieve
    local s_res = sieve(sievebuf, SIEVE_LIMIT)
    
    local stop = os.clock()
    
    print(string.format("Total Time: %.3f seconds", stop - start))
    print(string.format("Primes found: %d", s_res))
end

run_bench()
