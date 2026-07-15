/*
 * SPDX-License-Identifier: MIT
 *
 * Sprite-backed "familiar" avatar skin (PROTOCOL_V2 / WS4 F3).
 *
 * Unlike the procedural default skin, the eyes/mouth are sprite frames, but they
 * are still driven by the SAME animation modifiers: SpriteEyes/SpriteMouth pick a
 * frame from the Feature "weight" the modifiers set each tick, so BlinkModifier
 * (weight 25 -> closed) and SpeakingModifier (mouth weight 40..80 -> open) animate
 * the face exactly as they do the default. A static costume sprite (ears,
 * whiskers, head) sits behind the features to make it a recognizable animal.
 *
 * Asset names: "<name>_face.png", "<name>_eye_open.png", "<name>_eye_closed.png",
 * "<name>_mouth_closed.png", "<name>_mouth_open.png" — loaded from the assets
 * partition (see main/assets/familiars/gen_familiars.py for the placeholder cat).
 */
#pragma once
#include "../../avatar/avatar.h"
#include "../../avatar/elements/feature.h"
#include "../default/default.h"  // DefaultSpeechBubble
#include <lvgl.h>
#include <smooth_lvgl.hpp>
#include <memory>
#include <string>

namespace stackchan::avatar {

class SpriteEyes : public Feature {
public:
    SpriteEyes(lv_obj_t* parent, const std::string& prefix, bool isLeftEye);
    ~SpriteEyes();

    void setPosition(const uitk::Vector2i& position) override;
    void setWeight(int weight) override;
    void setRotation(int rotation) override;
    void setEmotion(const Emotion& emotion) override;
    void setVisible(bool visible) override;

private:
    void applyFrame();

    bool _is_left_eye = false;
    bool _open        = true;
    lv_image_dsc_t _open_dsc{};
    lv_image_dsc_t _closed_dsc{};
    std::unique_ptr<uitk::lvgl_cpp::Image> _img;
};

class SpriteMouth : public Feature {
public:
    SpriteMouth(lv_obj_t* parent, const std::string& prefix);
    ~SpriteMouth();

    void setPosition(const uitk::Vector2i& position) override;
    void setWeight(int weight) override;
    void setRotation(int rotation) override;
    void setVisible(bool visible) override;

private:
    void applyFrame();

    bool _open = false;
    lv_image_dsc_t _closed_dsc{};
    lv_image_dsc_t _open_dsc{};
    std::unique_ptr<uitk::lvgl_cpp::Image> _img;
};

class FamiliarAvatar : public Avatar {
public:
    explicit FamiliarAvatar(std::string name) : _name(std::move(name)) {}
    ~FamiliarAvatar() override
    {
        teardown();  // features/decorators go before _panel, their LVGL parent
    }

    void init(lv_obj_t* parent, const lv_font_t* font = &lv_font_montserrat_16);
    uitk::lvgl_cpp::Container* getPanel() const;

private:
    std::string _name;
    lv_image_dsc_t _face_dsc{};
    std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
    std::unique_ptr<uitk::lvgl_cpp::Image> _costume;
};

}  // namespace stackchan::avatar
