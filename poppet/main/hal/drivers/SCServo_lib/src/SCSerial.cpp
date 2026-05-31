/*
 * SCSerial.cpp
 * FIT serial servo hardware interface layer program
 */

#include "SCSerial.h"
#include "esp_log.h"

static const char *TAG = "SCSerial";

// IOTimeOut: per-read serial timeout in ms. A valid servo reply at 1 Mbaud is
// sub-millisecond; 10 ms is ample margin while bounding how long a missed/slow
// or unplugged-servo ACK can block the (now-blocking) read — at 100 ms a dead
// servo could stall the update task long enough to trip task_wdt.
SCSerial::SCSerial()
{
    IOTimeOut = 10;
    uart_num  = UART_NUM_MAX;
}

SCSerial::SCSerial(u8 End) : SCS(End)
{
    IOTimeOut = 10;
    uart_num  = UART_NUM_MAX;
}

SCSerial::SCSerial(u8 End, u8 Level) : SCS(End, Level)
{
    IOTimeOut = 10;
    uart_num  = UART_NUM_MAX;
}

bool SCSerial::begin(uart_port_t uart_num, int baud_rate, int tx_pin, int rx_pin, int buf_size)
{
    this->uart_num = uart_num;

    uart_config_t uart_config = {
        .baud_rate           = baud_rate,
        .data_bits           = UART_DATA_8_BITS,
        .parity              = UART_PARITY_DISABLE,
        .stop_bits           = UART_STOP_BITS_1,
        .flow_ctrl           = UART_HW_FLOWCTRL_DISABLE,
        .rx_flow_ctrl_thresh = 0,
        .source_clk          = UART_SCLK_DEFAULT,
    };

    esp_err_t ret = uart_driver_install(uart_num, buf_size, buf_size, 0, NULL, 0);
    if (ret != ESP_OK) {
        ESP_LOGE(TAG, "Failed to install UART driver: %s", esp_err_to_name(ret));
        return false;
    }

    ret = uart_param_config(uart_num, &uart_config);
    if (ret != ESP_OK) {
        ESP_LOGE(TAG, "Failed to configure UART: %s", esp_err_to_name(ret));
        uart_driver_delete(uart_num);
        return false;
    }

    ret = uart_set_pin(uart_num, tx_pin, rx_pin, UART_PIN_NO_CHANGE, UART_PIN_NO_CHANGE);
    if (ret != ESP_OK) {
        ESP_LOGE(TAG, "Failed to set UART pins: %s", esp_err_to_name(ret));
        uart_driver_delete(uart_num);
        return false;
    }

    return true;
}

void SCSerial::end()
{
    if (uart_num < UART_NUM_MAX) {
        uart_driver_delete(uart_num);
        uart_num = UART_NUM_MAX;
    }
}

int SCSerial::readSCS(unsigned char *nDat, int nLen)
{
    if (uart_num >= UART_NUM_MAX || nLen <= 0) {
        return 0;
    }

    // Blocking read: sleep on the UART driver's ISR-fed RX ring buffer until
    // nLen bytes arrive or IOTimeOut elapses, instead of busy-polling at task
    // priority. The previous implementation polled uart_get_buffered_data_len()
    // in a loop whose only yield was vTaskDelay(1 / portTICK_PERIOD_MS) — but
    // with CONFIG_FREERTOS_HZ=100 that integer-divides to vTaskDelay(0), a bare
    // yield. On a missed/slow servo ACK it spun the full IOTimeOut at priority 5
    // on Core 1, starving the opus codec task (priority 6, same core) and
    // corrupting TTS playback. Blocking here frees the CPU while we wait.
    // uart_read_bytes() returns the byte count actually read; callers treat a
    // short read as an error (checksum/length mismatch).
    const TickType_t timeout = pdMS_TO_TICKS(IOTimeOut);

    if (nDat) {
        int got = uart_read_bytes(uart_num, nDat, nLen, timeout);
        return got < 0 ? 0 : got;
    }

    // No destination buffer — drain and discard nLen bytes.
    unsigned char scratch[16];
    int total = 0;
    while (total < nLen) {
        int want = nLen - total;
        if (want > (int)sizeof(scratch)) {
            want = sizeof(scratch);
        }
        int got = uart_read_bytes(uart_num, scratch, want, timeout);
        if (got <= 0) {
            break;
        }
        total += got;
    }
    return total;
}

int SCSerial::writeSCS(unsigned char *nDat, int nLen)
{
    if (uart_num >= UART_NUM_MAX || nDat == NULL) {
        return 0;
    }

    int len = uart_write_bytes(uart_num, (const char *)nDat, nLen);
    return len;
}

int SCSerial::writeSCS(unsigned char bDat)
{
    if (uart_num >= UART_NUM_MAX) {
        return 0;
    }

    int len = uart_write_bytes(uart_num, (const char *)&bDat, 1);
    return len;
}

void SCSerial::rFlushSCS()
{
    if (uart_num >= UART_NUM_MAX) {
        return;
    }

    uart_flush_input(uart_num);
}

void SCSerial::wFlushSCS()
{
    if (uart_num >= UART_NUM_MAX) {
        return;
    }

    uart_wait_tx_done(uart_num, pdMS_TO_TICKS(100));
}