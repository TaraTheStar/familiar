/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once
#include "elements/key_elements.h"
#include "decorator.h"
#include <memory>

namespace stackchan::avatar {

/**
 * @brief Avatar base class
 *
 */
class Avatar {
public:
    // Skins are held and swapped through unique_ptr<Avatar> (set_familiar), so
    // destruction runs through this base pointer. Concrete skins that own an
    // LVGL panel must destroy the key elements/decorators BEFORE the panel in
    // their own destructor (see teardown() and ~FamiliarAvatar/~DefaultAvatar):
    // C++ destroys derived members (the panel) before base members (the
    // elements), and lv_obj_del on the panel frees the elements' child objects
    // first — their wrappers would then delete them a second time.
    virtual ~Avatar() = default;

    /**
     * @brief Update avatar, trigger all elements, decorators and modifiers to update
     *
     */
    virtual void update()
    {
        _key_elements.forEach([](Element* element) {
            // Update all elements
            element->_update();
        });

        _decorator_pool.forEach([this](Decorator* decorator, int id) {
            // Update all decorators
            decorator->_update();
        });

        // Cleanup pools
        _decorator_pool.cleanup();
    }

    const KeyElements_t& getKeyElements()
    {
        return _key_elements;
    }

    virtual void setEmotion(const Emotion& emotion)
    {
        _emotion = emotion;

        _key_elements.forEach([&emotion](Element* element) {
            // Set for all elements
            element->setEmotion(emotion);
        });

        _decorator_pool.forEach([&emotion](Decorator* decorator, int id) {
            // Set for all decorators
            decorator->setEmotion(emotion);
        });
    }

    Emotion getEmotion() const
    {
        return _emotion;
    }

    Feature& leftEye()
    {
        return *getKeyElements().leftEye;
    }

    Feature& rightEye()
    {
        return *getKeyElements().rightEye;
    }

    Feature& mouth()
    {
        return *getKeyElements().mouth;
    }

    void setSpeech(std::string_view text)
    {
        if (getKeyElements().speechBubble) {
            getKeyElements().speechBubble->setSpeech(text);
        }
    }

    void clearSpeech()
    {
        if (getKeyElements().speechBubble) {
            getKeyElements().speechBubble->clearSpeech();
        }
    }

    void setSpeechTextFont(void* font)
    {
        if (getKeyElements().speechBubble) {
            getKeyElements().speechBubble->setTextFont(font);
        }
    }

    void setModifyLock(bool locked)
    {
        _is_modify_locked = locked;
    }

    bool isModifyLocked()
    {
        return _is_modify_locked;
    }

    /* ---------------------------- Decorator helpers --------------------------- */

    int addDecorator(std::unique_ptr<Decorator> decorator)
    {
        return _decorator_pool.create(std::move(decorator));
    }

    bool removeDecorator(int id)
    {
        return _decorator_pool.destroy(id);
    }

    void clearDecorators()
    {
        _decorator_pool.clear();
    }

protected:
    Avatar() = default;

    // Destroy the base-owned pieces (features, speech bubble, decorators)
    // while the derived skin's panel — their LVGL parent — is still alive.
    // Call first thing from the concrete skin's destructor.
    void teardown()
    {
        _decorator_pool.clear();
        _key_elements = {};
    }

    Emotion _emotion = Emotion::Neutral;
    KeyElements_t _key_elements;
    ObjectPool<Decorator> _decorator_pool;

    bool _is_modify_locked = false;
};

}  // namespace stackchan::avatar
