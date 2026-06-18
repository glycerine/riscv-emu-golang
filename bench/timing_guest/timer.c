/*
. Check the CLINT (Core Local Interruptor)
The time CSR is linked to the mtime and mtimecmp registers in the CLINT.

The kernel sets mtimecmp to a future value and expects the timer interrupt to trigger when mtime >= mtimecmp.

If your emulator doesn't have an emulated CLINT that updates mtime and checks against mtimecmp to trigger a Machine Timer Interrupt, the kernel will hang forever waiting for that first interrupt.

3. Quick "Hack" to verify
To see if this is the issue, you can try disabling the timer dependency by adding clocksource=dummy to your kernel boot arguments.

Note: This is a debugging step, not a permanent fix. If it boots faster, it confirms that your timer/interrupt infrastructure is failing to provide a signal to the kernel.
*/
#include <stdio.h>

unsigned long long get_time() {
    unsigned long long val;
    __asm__ volatile ("rdtime %0" : "=r" (val));
    return val;
}

int main() {
    while(1) {
        printf("Current time: %llu\n", get_time());
    }
}
