/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once
#include "../modifiable.h"
#include "../utils/random.h"
#include <hal/hal.h>
#include <cstdint>

namespace stackchan {

/**
 * @brief
 *
 */
class BlinkModifier : public Modifier {
public:
    /**
     * @param destroyAfterMs How long before blinking stops and the modifier is destroyed (0 = forever)
     * @param openIntervalMs Duration the eyes stay open
     * @param closeIntervalMs Duration the eyes stay closed (momentary)
     */
    BlinkModifier(uint32_t destroyAfterMs = 0, uint32_t openIntervalMs = 5200, uint32_t closeIntervalMs = 200)
        : _open_interval_ms(openIntervalMs), _close_interval_ms(closeIntervalMs)
    {
        uint32_t now = GetHAL().millis();

        // Handle destroy timing
        if (destroyAfterMs > 0) {
            _destroy_at   = now + destroyAfterMs;
            _has_lifetime = true;
        }

        // Initialize: start from the open state, ready to close
        _state           = State::OPEN;
        _next_state_tick = now + _open_interval_ms;
    }

    void resyncEyeWeights()
    {
        _needs_resync = true;
    }

    void _update(Modifiable& stackchan) override
    {
        if (!stackchan.hasAvatar() || stackchan.avatar().isModifyLocked()) {
            return;
        }

        uint32_t now = GetHAL().millis();

        // 1. Handle destroy logic
        if (_has_lifetime && now >= _destroy_at) {
            // Make sure the eyes are open before destroying
            if (_state == State::CLOSED) {
                apply_eye_weights(stackchan, _left_eye_weight, _right_eye_weight);
            }
            requestDestroy();
            return;
        }

        // 2. Handle weight resync requests
        // If the eyes are currently closed, we just record the weights and apply them when they open
        if (_needs_resync) {
            _needs_resync     = false;
            _left_eye_weight  = stackchan.avatar().leftEye().getWeight();
            _right_eye_weight = stackchan.avatar().rightEye().getWeight();
        }

        // 3. State machine transition logic
        if (now >= _next_state_tick) {
            if (_state == State::OPEN) {
                // Open -> Closed
                _state           = State::CLOSED;
                _next_state_tick = now + _close_interval_ms;

                // At the moment of closing, back up the current weights (in case they were modified externally)
                _left_eye_weight  = stackchan.avatar().leftEye().getWeight();
                _right_eye_weight = stackchan.avatar().rightEye().getWeight();

                apply_eye_weights(stackchan, 25, 25);
            } else {
                // Closed -> Open
                _state = State::OPEN;
                // Add a little random jitter to the open time so it looks more natural
                uint32_t jitter  = Random::getInstance().getInt(0, 500);
                _next_state_tick = now + _open_interval_ms + jitter;

                apply_eye_weights(stackchan, _left_eye_weight, _right_eye_weight);
            }
        }
    }

private:
    enum class State { OPEN, CLOSED };

    void apply_eye_weights(Modifiable& stackchan, int left, int right)
    {
        stackchan.avatar().leftEye().setWeight(left);
        stackchan.avatar().rightEye().setWeight(right);
    }

    State _state;
    uint32_t _next_state_tick = 0;
    uint32_t _open_interval_ms;
    uint32_t _close_interval_ms;

    uint32_t _destroy_at  = 0;
    bool _has_lifetime    = false;
    bool _needs_resync    = false;
    int _left_eye_weight  = 100;
    int _right_eye_weight = 100;
};

}  // namespace stackchan
