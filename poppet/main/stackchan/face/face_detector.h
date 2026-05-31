/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once
#include <freertos/FreeRTOS.h>
#include <freertos/semphr.h>
#include <freertos/task.h>
#include <atomic>
#include <cstdint>

namespace stackchan {

class FaceDetector {
public:
    static FaceDetector& getInstance();

    void start();
    void stop();
    void setEnabled(bool enabled);
    bool isEnabled() const { return _enabled.load(std::memory_order_acquire); }

private:
    FaceDetector() = default;
    static void taskEntry(void* arg);
    // Runs one detection pass. Returns true iff a face was detected this frame
    // (false on no-face *and* on early-out when the camera/arbiter was busy).
    // Drives the adaptive cadence in taskEntry.
    bool processFrame();

    TaskHandle_t _task_handle = nullptr;
    std::atomic<bool> _enabled{false};
    std::atomic<bool> _running{false};
    SemaphoreHandle_t _stop_sem = nullptr;
};

}  // namespace stackchan
