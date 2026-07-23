/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "decorators.h"
#include <hal/hal.h>
#include <vector>

using namespace uitk;
using namespace uitk::lvgl_cpp;
using namespace stackchan::avatar;

static const Vector2i _dizzy_left_default_position  = Vector2i(-70, -16);
static const Vector2i _dizzy_right_default_position = Vector2i(70, -16);

static const lv_color_t _dizzy_default_color = lv_color_hex(0xFFFFFF);

LV_IMAGE_DECLARE(decorator_dizzy);

DizzyDecorator::DizzyDecorator(lv_obj_t* parent, uint32_t destroyAfterMs, uint32_t animationIntervalMs)
    : _animation_interval_ms(animationIntervalMs)
{
    // Initialize Left Image
    _left = std::make_unique<Image>(parent);
    _left->setSrc(&decorator_dizzy);
    _left->setAlign(LV_ALIGN_CENTER);
    _left->setPos(_dizzy_left_default_position.x, _dizzy_left_default_position.y);
    _left->setTransformPivot(_left->getWidth() / 2, _left->getHeight() / 2);
    _left->setRotation(0);
    _left->setImageRecolorOpa(LV_OPA_COVER);
    _left->setImageRecolor(_dizzy_default_color);

    // Initialize Right Image
    _right = std::make_unique<Image>(parent);
    _right->setSrc(&decorator_dizzy);
    _right->setAlign(LV_ALIGN_CENTER);
    _right->setPos(_dizzy_right_default_position.x, _dizzy_right_default_position.y);
    _right->setTransformPivot(_right->getWidth() / 2, _right->getHeight() / 2);
    _right->setRotation(450);
    _right->setImageRecolorOpa(LV_OPA_COVER);
    _right->setImageRecolor(_dizzy_default_color);

    uint32_t now = GetHAL().millis();

    if (destroyAfterMs > 0) {
        _destroy_at   = now + destroyAfterMs;
        _has_lifetime = true;
    }

    if (_animation_interval_ms > 0) {
        _next_animation_tick = now + _animation_interval_ms;
    }

    _animation_index = 0;  // Used as the current rotation angle
}

DizzyDecorator::~DizzyDecorator()
{
}

void DizzyDecorator::_update()
{
    uint32_t now = GetHAL().millis();

    if (_has_lifetime && now >= _destroy_at) {
        requestDestroy();
        return;
    }

    if (_animation_interval_ms > 0 && now >= _next_animation_tick) {
        _next_animation_tick = now + _animation_interval_ms;

        // Increment by 30 degrees (300 units)
        _animation_index = (_animation_index + 300) % 3600;
        int rotation     = -_animation_index;

        if (_left) {
            _left->setRotation(rotation);
        }
        if (_right) {
            _right->setRotation((rotation + 450) % 3600);
        }
    }
}

void DizzyDecorator::setPosition(int x, int y)
{
    if (_left) {
        _left->setPos(x + _dizzy_left_default_position.x, y + _dizzy_left_default_position.y);
    }
    if (_right) {
        _right->setPos(x + _dizzy_right_default_position.x, y + _dizzy_right_default_position.y);
    }
}

void DizzyDecorator::setRotation(int rotation)
{
    // Note: a manually set rotation may be overwritten by the animation in update()
    // But following the HeartDecorator logic, setRotation also sets it directly
    if (_left) {
        _left->setRotation(rotation);
    }
    if (_right) {
        _right->setRotation(rotation);
    }
}

void DizzyDecorator::setColor(lv_color_t color)
{
    if (_left) {
        _left->setImageRecolor(color);
    }
    if (_right) {
        _right->setImageRecolor(color);
    }
}

void DizzyDecorator::setVisible(bool visible)
{
    if (_left) {
        _left->setHidden(!visible);
    }
    if (_right) {
        _right->setHidden(!visible);
    }
}
