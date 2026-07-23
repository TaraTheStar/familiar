
/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once
#include <vector>
#include <memory>
#include <functional>

namespace stackchan {

class Poolable {
public:
    virtual ~Poolable() = default;

    // Mark the object for destruction.
    // Typically called by the object itself (e.g., inside its Update method).
    void requestDestroy()
    {
        _destroy_requested = true;
    }

    bool isDestroyRequested() const
    {
        return _destroy_requested;
    }

private:
    bool _destroy_requested = false;
};

/**
 * @brief Generic object pool
 * Suited to scenarios that do not care about a maximum capacity and instead
 * focus on managing each object's own lifecycle.
 *
 * @tparam T
 */
template <typename T>
class ObjectPool {
public:
    /**
     * @brief Create a new object and return its ID.
     * Uses a free-list strategy to achieve O(1) insertion.
     *
     * @param obj Unique pointer to the object instance
     * @return int Allocated object ID (index)
     */
    int create(std::unique_ptr<T> obj)
    {
        // 1. If a free slot is available, reuse it
        if (!_free_indices.empty()) {
            int id = _free_indices.back();
            _free_indices.pop_back();
            _objs[id] = std::move(obj);
            return id;
        }

        // 2. No free slot, append to the end
        _objs.push_back(std::move(obj));
        return (int)_objs.size() - 1;
    }

    /**
     * @brief Get an object instance by ID.
     *
     * @param id Object ID
     * @return T* Object pointer, or nullptr if invalid or empty
     */
    T* get(int id)
    {
        if (id < 0 || id >= (int)_objs.size()) {
            return nullptr;
        }
        return static_cast<T*>(_objs[id].get());
    }

    /**
     * @brief Iterate over all active objects.
     *
     * @param func Callback receiving (Object*, ID)
     */
    void forEach(const std::function<void(T*, int)>& func)
    {
        for (int i = 0; i < (int)_objs.size(); ++i) {
            if (_objs[i]) {
                func(static_cast<T*>(_objs[i].get()), i);
            }
        }
    }

    /**
     * @brief Main adapter method for Poolable:
     * Checks every object to see whether it has requested destruction.
     * Should be called once per frame (e.g., at the end of the game loop).
     */
    void cleanup()
    {
        for (int i = 0; i < (int)_objs.size(); ++i) {
            // If the object exists and has requested destruction
            if (_objs[i] && _objs[i]->isDestroyRequested()) {
                destroy(i);
            }
        }
    }

    /**
     * @brief Destroy an object by ID and free its slot.
     *
     * @param id Object ID
     * @return true if destroyed successfully, false if invalid or already empty
     */
    bool destroy(int id)
    {
        if (id < 0 || id >= (int)_objs.size()) {
            return false;
        }

        // Prevent double free
        if (!_objs[id]) {
            return false;
        }

        // Release memory
        _objs[id].reset();

        // Add this ID to the free list for immediate reuse
        _free_indices.push_back(id);

        return true;
    }

    /**
     * @brief Clear all objects and reset the pool.
     */
    void clear()
    {
        _objs.clear();
        _free_indices.clear();
    }

    /**
     * @brief Get the total capacity (max index + 1).
     * Note: this includes empty slots (holes).
     *
     * @return size_t
     */
    size_t size()
    {
        return _objs.size();
    }

    /**
     * @brief Get the actual number of active objects.
     *
     * @return size_t
     */
    size_t activeCount()
    {
        return _objs.size() - _free_indices.size();
    }

private:
    std::vector<std::unique_ptr<Poolable>> _objs;
    std::vector<int> _free_indices;
};

/**
 * @brief Ring object pool
 * Suited to scenarios with many objects and a fixed count (fixed length).
 * When the pool is full, new objects automatically overwrite the oldest ones.
 *
 * @tparam T
 */
template <typename T>
class RingObjectPool {
public:
    /**
     * @brief
     * @param capacity Maximum capacity of the pool
     */
    explicit RingObjectPool(size_t capacity) : _capacity(capacity), _cursor(0)
    {
        // Pre-allocate all slots, all initialized to nullptr
        _objs.resize(capacity);
    }

    /**
     * @brief Create/spawn a new object.
     * If the current slot already holds an object, that old object is
     * destructed immediately (overwritten).
     *
     * @param obj Unique pointer to the new object
     * @return int Allocated ID (index)
     */
    int create(std::unique_ptr<T> obj)
    {
        int id = _cursor;

        // If this slot already holds an object, the unique_ptr assignment
        // automatically destructs the old object.
        // This step implements "automatically replace the oldest data".
        _objs[id] = std::move(obj);

        // Advance the cursor, wrapping back to 0 when it exceeds capacity
        _cursor = (_cursor + 1) % _capacity;

        return id;
    }

    /**
     * @brief Get an object by ID
     * Note: in a ring pool, if you hold an ID for too long, the content at that
     * ID may already have been overwritten by a newer object.
     *
     * @param id Object index
     * @return T* Object pointer, or nullptr if the slot is empty
     */
    T* get(int id)
    {
        if (id < 0 || id >= (int)_objs.size()) {
            return nullptr;
        }
        return _objs[id].get();
    }

    /**
     * @brief Iterate over all active objects
     * Automatically skips empty slots (nullptr)
     */
    void forEach(const std::function<void(T*, int)>& func)
    {
        for (int i = 0; i < (int)_objs.size(); ++i) {
            if (_objs[i]) {
                func(_objs[i].get(), i);
            }
        }
    }

    /**
     * @brief Clean up objects that have actively requested destruction.
     * Targets objects that are not yet due to be overwritten but are logically
     * finished (e.g., an enemy whose health has reached zero).
     */
    void cleanup()
    {
        for (auto& ptr : _objs) {
            // If the object exists and has requested destruction
            if (ptr && ptr->isDestroyRequested()) {
                ptr.reset();  // Release memory, becomes an empty slot (nullptr)
            }
        }
    }

    /**
     * @brief Get the total capacity
     */
    size_t capacity() const
    {
        return _capacity;
    }

    /**
     * @brief Get the current number of actually alive objects
     */
    size_t activeCount() const
    {
        size_t count = 0;
        for (const auto& ptr : _objs) {
            if (ptr) count++;
        }
        return count;
    }

    /**
     * @brief Force-clear the entire pool
     */
    void clear()
    {
        for (auto& ptr : _objs) {
            ptr.reset();
        }
        _cursor = 0;  // Reset the cursor
    }

private:
    std::vector<std::unique_ptr<T>> _objs;
    size_t _capacity;
    int _cursor;  // Pointer to the current write position
};

}  // namespace stackchan
