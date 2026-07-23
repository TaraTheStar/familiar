/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once
#include "../modifiable.h"
#include <hal/hal.h>
#include <cstdint>
#include <cmath>

namespace stackchan {

/**
 * @brief
 *
 */
class BreathModifier : public Modifier {
public:
    /**
     * @param destroyAfterMs Duration (0 = forever)
     * @param amplitude Breathing amplitude, in pixels
     * @param breathCycleMs Period of one breath (inhale + exhale)
     * @param updateIntervalMs Update interval
     */
    BreathModifier(uint32_t destroyAfterMs = 0, int amplitude = 16, uint32_t breathCycleMs = 6600,
                   uint32_t updateIntervalMs = 600)
        : _amplitude(amplitude), _breath_cycle_ms(breathCycleMs), _update_interval_ms(updateIntervalMs)
    {
        _start_tick = GetHAL().millis();
        if (destroyAfterMs > 0) {
            _destroy_at   = _start_tick + destroyAfterMs;
            _has_lifetime = true;
        }
    }

    void _update(Modifiable& stackchan) override
    {
        if (!stackchan.hasAvatar()) return;

        uint32_t now = GetHAL().millis();

        // Destroy logic
        if (_has_lifetime && now >= _destroy_at) {
            reset_position(stackchan.avatar());  // Reset position before destroying
            requestDestroy();
            return;
        }

        if (now - _last_update_tick < _update_interval_ms) {
            return;
        }
        _last_update_tick = now;

        // Use a sine wave to calculate the offset
        // (now - _start_tick) / cycle gives progress, multiplied by 2PI and passed to sin
        float phase   = (float)((now - _start_tick) % _breath_cycle_ms) / _breath_cycle_ms;
        float sin_val = sinf(phase * 2.0f * M_PI);

        // Calculate the current offset
        int current_offset = static_cast<int>(sin_val * _amplitude);

        // Apply the incremental offset
        apply_relative_offset(stackchan.avatar(), current_offset);
    }

private:
    void apply_relative_offset(avatar::Avatar& avatar, int new_offset)
    {
        // Calculate the delta to move this time
        int delta = new_offset - _last_applied_offset;
        if (delta == 0) return;

        // Move the facial features together
        move_component(avatar.leftEye(), delta);
        move_component(avatar.rightEye(), delta);
        move_component(avatar.mouth(), delta);

        _last_applied_offset = new_offset;
    }

    void move_component(avatar::Feature& comp, int delta_y)
    {
        auto pos = comp.getPosition();
        pos.y += delta_y;
        comp.setPosition(pos);
    }

    void reset_position(avatar::Avatar& avatar)
    {
        // Zero out the offset
        apply_relative_offset(avatar, 0);
    }

    int _amplitude;
    uint32_t _breath_cycle_ms;
    uint32_t _update_interval_ms;
    uint32_t _start_tick       = 0;
    uint32_t _last_update_tick = 0;
    uint32_t _destroy_at       = 0;
    bool _has_lifetime         = false;

    int _last_applied_offset = 0;  // Records the last applied offset, used for incremental updates
};

}  // namespace stackchan
