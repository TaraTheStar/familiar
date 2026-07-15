#include "application.h"
#include "board.h"
#include "display.h"
#include "system_info.h"
#include "audio_codec.h"
#include "mqtt_protocol.h"
#include "websocket_protocol.h"
#include "assets/lang_config.h"
#include "mcp_server.h"
#include "assets.h"
#include "settings.h"

#include <cstring>
#include <esp_log.h>
#include <cJSON.h>
#include <driver/gpio.h>
#include <arpa/inet.h>
#include <font_awesome.h>

#define TAG "Application"


Application::Application() {
    event_group_ = xEventGroupCreate();

#if CONFIG_USE_DEVICE_AEC && CONFIG_USE_SERVER_AEC
#error "CONFIG_USE_DEVICE_AEC and CONFIG_USE_SERVER_AEC cannot be enabled at the same time"
#elif CONFIG_USE_DEVICE_AEC
    aec_mode_ = kAecOnDeviceSide;
#elif CONFIG_USE_SERVER_AEC
    aec_mode_ = kAecOnServerSide;
#else
    aec_mode_ = kAecOff;
#endif

    esp_timer_create_args_t clock_timer_args = {
        .callback = [](void* arg) {
            Application* app = (Application*)arg;
            xEventGroupSetBits(app->event_group_, MAIN_EVENT_CLOCK_TICK);
        },
        .arg = this,
        .dispatch_method = ESP_TIMER_TASK,
        .name = "clock_timer",
        .skip_unhandled_events = true
    };
    esp_timer_create(&clock_timer_args, &clock_timer_handle_);
}

Application::~Application() {
    if (clock_timer_handle_ != nullptr) {
        esp_timer_stop(clock_timer_handle_);
        esp_timer_delete(clock_timer_handle_);
    }
    vEventGroupDelete(event_group_);
}

bool Application::SetDeviceState(DeviceState state) {
    // Privacy gate (sleep): refuse only the transitions that ARM THE MIC
    // (Connecting/Listening). Speaking is allowed — it doesn't use the mic —
    // so a farewell ("Goodnight!") can still be heard before the device goes
    // quiet; tts:stop then routes Speaking back to Idle (see OnIncomingJson)
    // instead of Listening, so the mic never comes back on while gated.
    if (privacy_gate_ &&
        (state == kDeviceStateListening || state == kDeviceStateConnecting)) {
        ESP_LOGI(TAG, "privacy gate on: ignoring transition to %s",
                 state_machine_.GetStateName(state));
        return false;
    }
    return state_machine_.TransitionTo(state);
}

// SetPrivacyGate enables/disables the sleep-mode privacy gate. When enabled,
// the microphone and wake-word detection are turned off and the device is
// pinned to Idle (see SetDeviceState + HandleStateChangedEvent). Called by
// the StackChan state manager on sleep entry/exit.
void Application::SetPrivacyGate(bool on) {
    // The flag flips immediately (SetDeviceState reads it from any task, and
    // sleep entry must refuse mic-arming transitions from this moment), but
    // the AFE pipeline mutations are marshalled onto the main task:
    // EnableVoiceProcessing / EnableWakeWordDetection have no internal
    // locking, and the main task drives the same calls from
    // HandleStateChangedEvent — while our callers (StateManager sleep
    // entry/exit, e.g. the head-pet path) arrive on the stackchan update task.
    if (privacy_gate_.exchange(on) == on) {
        return;
    }
    Schedule([this, on]() {
        if (on) {
            ESP_LOGI(TAG, "privacy gate ON: mic + wake word disabled");
            // Drop out of any conversation. If we were Listening/Speaking this
            // transition fires HandleStateChangedEvent's gated Idle path (mic +
            // wake off, display left alone). If we were already Idle the
            // transition is a no-op, so disable here too.
            SetDeviceState(kDeviceStateIdle);
            audio_service_.EnableVoiceProcessing(false);
            audio_service_.EnableWakeWordDetection(false);
        } else {
            ESP_LOGI(TAG, "privacy gate OFF: re-arming wake word");
            // We're pinned to Idle, so SetDeviceState(Idle) would be a no-op and
            // wouldn't re-fire the listener — re-arm wake word detection directly.
            audio_service_.EnableWakeWordDetection(true);
        }
    });
}

void Application::Initialize() {
    auto& board = Board::GetInstance();
    SetDeviceState(kDeviceStateStarting);

    // Setup the display
    auto display = board.GetDisplay();
    display->SetupUI();
    // Print board name/version info
    display->SetChatMessage("system", SystemInfo::GetUserAgent().c_str());

    // Setup the audio service
    auto codec = board.GetAudioCodec();
    audio_service_.Initialize(codec);
    audio_service_.Start();

    AudioServiceCallbacks callbacks;
    callbacks.on_send_queue_available = [this]() {
        xEventGroupSetBits(event_group_, MAIN_EVENT_SEND_AUDIO);
    };
    callbacks.on_wake_word_detected = [this](const std::string& wake_word) {
        xEventGroupSetBits(event_group_, MAIN_EVENT_WAKE_WORD_DETECTED);
    };
    callbacks.on_vad_change = [this](bool speaking) {
        xEventGroupSetBits(event_group_, MAIN_EVENT_VAD_CHANGE);
    };
    audio_service_.SetCallbacks(callbacks);

    // Add state change listeners
    state_machine_.AddStateChangeListener([this](DeviceState old_state, DeviceState new_state) {
        xEventGroupSetBits(event_group_, MAIN_EVENT_STATE_CHANGED);
    });

    // Start the clock timer to update the status bar
    esp_timer_start_periodic(clock_timer_handle_, 1000000);

    // Add MCP common tools (only once during initialization)
    auto& mcp_server = McpServer::GetInstance();
    mcp_server.AddCommonTools();
    mcp_server.AddUserOnlyTools();

    // Set network event callback for UI updates and network state handling
    board.SetNetworkEventCallback([this](NetworkEvent event, const std::string& data) {
        auto display = Board::GetInstance().GetDisplay();
        
        switch (event) {
            case NetworkEvent::Scanning:
                display->ShowNotification(Lang::Strings::SCANNING_WIFI, 30000);
                xEventGroupSetBits(event_group_, MAIN_EVENT_NETWORK_DISCONNECTED);
                break;
            case NetworkEvent::Connecting: {
                if (data.empty()) {
                    // Cellular network - registering without carrier info yet
                    display->SetStatus(Lang::Strings::REGISTERING_NETWORK);
                } else {
                    // WiFi or cellular with carrier info
                    std::string msg = Lang::Strings::CONNECT_TO;
                    msg += data;
                    msg += "...";
                    display->ShowNotification(msg.c_str(), 30000);
                }
                break;
            }
            case NetworkEvent::Connected: {
                std::string msg = Lang::Strings::CONNECTED_TO;
                msg += data;
                display->ShowNotification(msg.c_str(), 30000);
                xEventGroupSetBits(event_group_, MAIN_EVENT_NETWORK_CONNECTED);
                break;
            }
            case NetworkEvent::Disconnected:
                xEventGroupSetBits(event_group_, MAIN_EVENT_NETWORK_DISCONNECTED);
                break;
            case NetworkEvent::WifiConfigModeEnter:
                // WiFi config mode enter is handled by WifiBoard internally
                break;
            case NetworkEvent::WifiConfigModeExit:
                // WiFi config mode exit is handled by WifiBoard internally
                break;
            // Cellular modem specific events
            case NetworkEvent::ModemDetecting:
                display->SetStatus(Lang::Strings::DETECTING_MODULE);
                break;
            case NetworkEvent::ModemErrorNoSim:
                Alert(Lang::Strings::ERROR, Lang::Strings::PIN_ERROR, "triangle_exclamation", Lang::Sounds::OGG_ERR_PIN);
                break;
            case NetworkEvent::ModemErrorRegDenied:
                Alert(Lang::Strings::ERROR, Lang::Strings::REG_ERROR, "triangle_exclamation", Lang::Sounds::OGG_ERR_REG);
                break;
            case NetworkEvent::ModemErrorInitFailed:
                Alert(Lang::Strings::ERROR, Lang::Strings::MODEM_INIT_ERROR, "triangle_exclamation", Lang::Sounds::OGG_EXCLAMATION);
                break;
            case NetworkEvent::ModemErrorTimeout:
                display->SetStatus(Lang::Strings::REGISTERING_NETWORK);
                break;
        }
    });

    // Start network asynchronously
    board.StartNetwork();

    // Update the status bar immediately to show the network state
    display->UpdateStatusBar(true);
}

void Application::Run() {
    // Set the priority of the main task to 10
    vTaskPrioritySet(nullptr, 10);

    const EventBits_t ALL_EVENTS = 
        MAIN_EVENT_SCHEDULE |
        MAIN_EVENT_SEND_AUDIO |
        MAIN_EVENT_WAKE_WORD_DETECTED |
        MAIN_EVENT_VAD_CHANGE |
        MAIN_EVENT_CLOCK_TICK |
        MAIN_EVENT_ERROR |
        MAIN_EVENT_NETWORK_CONNECTED |
        MAIN_EVENT_NETWORK_DISCONNECTED |
        MAIN_EVENT_TOGGLE_CHAT |
        MAIN_EVENT_START_LISTENING |
        MAIN_EVENT_STOP_LISTENING |
        MAIN_EVENT_ACTIVATION_DONE |
        MAIN_EVENT_STATE_CHANGED;

    while (true) {
        auto bits = xEventGroupWaitBits(event_group_, ALL_EVENTS, pdTRUE, pdFALSE, portMAX_DELAY);

        if (bits & MAIN_EVENT_ERROR) {
            SetDeviceState(kDeviceStateIdle);
            Alert(Lang::Strings::ERROR, last_error_message_.c_str(), "circle_xmark", Lang::Sounds::OGG_EXCLAMATION);
        }

        if (bits & MAIN_EVENT_NETWORK_CONNECTED) {
            HandleNetworkConnectedEvent();
        }

        if (bits & MAIN_EVENT_NETWORK_DISCONNECTED) {
            HandleNetworkDisconnectedEvent();
        }

        if (bits & MAIN_EVENT_ACTIVATION_DONE) {
            HandleActivationDoneEvent();
        }

        if (bits & MAIN_EVENT_STATE_CHANGED) {
            HandleStateChangedEvent();
        }

        if (bits & MAIN_EVENT_TOGGLE_CHAT) {
            HandleToggleChatEvent();
        }

        if (bits & MAIN_EVENT_START_LISTENING) {
            HandleStartListeningEvent();
        }

        if (bits & MAIN_EVENT_STOP_LISTENING) {
            HandleStopListeningEvent();
        }

        if (bits & MAIN_EVENT_SEND_AUDIO) {
            while (auto packet = audio_service_.PopPacketFromSendQueue()) {
                if (protocol_ && !protocol_->SendAudio(std::move(packet))) {
                    break;
                }
            }
        }

        if (bits & MAIN_EVENT_WAKE_WORD_DETECTED) {
            HandleWakeWordDetectedEvent();
        }

        if (bits & MAIN_EVENT_VAD_CHANGE) {
            if (GetDeviceState() == kDeviceStateListening) {
                auto led = Board::GetInstance().GetLed();
                led->OnStateChanged();
            }
        }

        if (bits & MAIN_EVENT_SCHEDULE) {
            std::unique_lock<std::mutex> lock(mutex_);
            auto tasks = std::move(main_tasks_);
            lock.unlock();
            for (auto& task : tasks) {
                task();
            }
        }

        if (bits & MAIN_EVENT_CLOCK_TICK) {
            clock_ticks_++;
            auto display = Board::GetInstance().GetDisplay();
            display->UpdateStatusBar();

            // Print debug info every 10 seconds
            if (clock_ticks_ % 10 == 0) {
                SystemInfo::PrintHeapStats();
            }

            // battery_low telemetry (PROTOCOL_V2 §4.8): the one event the
            // server acts on (it answers with a "charge me" alert). Checked
            // once a minute; emitted once per discharge-crossing below the
            // threshold, re-armed by charging or recovering above the
            // hysteresis band so it doesn't spam on a level wobbling at the
            // threshold. SendEvent lazy-opens the channel from idle, which is
            // exactly what we want — a sleeping-on-the-shelf robot can still
            // call for its charger.
            if (clock_ticks_ % 60 == 0) {
                int level = 0;
                bool charging = false, discharging = false;
                auto& board = Board::GetInstance();
                if (board.GetBatteryLevel(level, charging, discharging)) {
                    constexpr int kBatteryLowPercent = 20;
                    constexpr int kBatteryRearmPercent = 25;
                    if (charging || level >= kBatteryRearmPercent) {
                        battery_low_sent_ = false;
                    } else if (level <= kBatteryLowPercent && !battery_low_sent_) {
                        battery_low_sent_ = true;
                        SendEvent("battery_low",
                                  "{\"percent\":" + std::to_string(level) + "}");
                    }
                }
            }

            // Dotty: proactive WS reconnect on idle-channel timeout.
            // IsTimeout() flips at 120 s of silence on the channel, but
            // the OS-level TCP teardown can take another ~45 s on top of
            // that, leaving the device disconnected and unresponsive to
            // walk-up events until a manual nudge re-opens the channel.
            // While idle, eagerly close+reopen so the next SendEvent /
            // wake-word / listen path finds a live channel waiting.
            //
            // Close+open run inline on this (main) task and can block for
            // seconds while the server is unreachable (connect + hello wait),
            // stalling wake-word/tool/schedule processing — so back off
            // exponentially on consecutive failures (30 s → 60 → 120 → … →
            // 480 s cap) instead of stalling every 30 s until the server
            // returns. Any success resets the backoff.
            if (clock_ticks_ % 30 == 0 &&
                GetDeviceState() == kDeviceStateIdle &&
                protocol_ != nullptr &&
                protocol_->IsTimeout()) {
                if (idle_reconnect_holdoff_ticks_ > 0) {
                    idle_reconnect_holdoff_ticks_ -= 30;
                } else {
                    ESP_LOGW(TAG, "Idle channel timeout — proactive close+reopen");
                    protocol_->CloseAudioChannel();
                    if (protocol_->OpenAudioChannel()) {
                        idle_reconnect_failures_ = 0;
                    } else {
                        idle_reconnect_failures_++;
                        int shift = idle_reconnect_failures_ < 4 ? idle_reconnect_failures_ : 4;
                        idle_reconnect_holdoff_ticks_ = 30 * (1 << shift);
                        ESP_LOGW(TAG, "Idle channel reopen failed; next retry in %d s",
                                 idle_reconnect_holdoff_ticks_ + 30);
                    }
                }
            }
        }
    }
}

void Application::HandleNetworkConnectedEvent() {
    ESP_LOGI(TAG, "Network connected");
    auto state = GetDeviceState();

    if (state == kDeviceStateStarting || state == kDeviceStateWifiConfiguring) {
        // Network is ready, start activation
        SetDeviceState(kDeviceStateActivating);
        if (activation_task_handle_ != nullptr) {
            ESP_LOGW(TAG, "Activation task already running");
            return;
        }

        xTaskCreate([](void* arg) {
            Application* app = static_cast<Application*>(arg);
            app->ActivationTask();
            app->activation_task_handle_ = nullptr;
            vTaskDelete(NULL);
        }, "activation", 4096 * 2, this, 2, &activation_task_handle_);
    }

    // Update the status bar immediately to show the network state
    auto display = Board::GetInstance().GetDisplay();
    display->UpdateStatusBar(true);
}

void Application::HandleNetworkDisconnectedEvent() {
    // Close current conversation when network disconnected
    auto state = GetDeviceState();
    if (state == kDeviceStateConnecting || state == kDeviceStateListening || state == kDeviceStateSpeaking) {
        ESP_LOGI(TAG, "Closing audio channel due to network disconnection");
        protocol_->CloseAudioChannel();
    }

    // Update the status bar immediately to show the network state
    auto display = Board::GetInstance().GetDisplay();
    display->UpdateStatusBar(true);
}

void Application::HandleActivationDoneEvent() {
    ESP_LOGI(TAG, "Activation done");

    SystemInfo::PrintHeapStats();
    SetDeviceState(kDeviceStateIdle);

    has_server_time_ = ota_->HasServerTime();

    auto display = Board::GetInstance().GetDisplay();
    std::string message = std::string(Lang::Strings::VERSION) + ota_->GetCurrentVersion();
    display->ShowNotification(message.c_str());
    display->SetChatMessage("system", "");

    // Release OTA object after activation is complete
    ota_.reset();
    auto& board = Board::GetInstance();
    board.SetPowerSaveLevel(PowerSaveLevel::LOW_POWER);

    Schedule([this]() {
        // Play the success sound to indicate the device is ready
        audio_service_.PlaySound(Lang::Sounds::OGG_SUCCESS);
    });
}

void Application::ActivationTask() {
    // Create OTA object for activation process
    ota_ = std::make_unique<Ota>();

    // Check for new assets version
    CheckAssetsVersion();

    // Check for new firmware version
    CheckNewVersion();

    // Initialize the protocol
    InitializeProtocol();

    // Signal completion to main loop
    xEventGroupSetBits(event_group_, MAIN_EVENT_ACTIVATION_DONE);
}

void Application::CheckAssetsVersion() {
    // Only allow CheckAssetsVersion to be called once
    if (assets_version_checked_) {
        return;
    }
    assets_version_checked_ = true;

    auto& board = Board::GetInstance();
    auto display = board.GetDisplay();
    auto& assets = Assets::GetInstance();

    if (!assets.partition_valid()) {
        ESP_LOGW(TAG, "Assets partition is disabled for board %s", BOARD_NAME);
        return;
    }
    
    Settings settings("assets", true);
    // Check if there is a new assets need to be downloaded
    std::string download_url = settings.GetString("download_url");

    if (!download_url.empty()) {
        settings.EraseKey("download_url");

        char message[256];
        snprintf(message, sizeof(message), Lang::Strings::FOUND_NEW_ASSETS, download_url.c_str());
        Alert(Lang::Strings::LOADING_ASSETS, message, "cloud_arrow_down", Lang::Sounds::OGG_UPGRADE);
        
        // Wait for the audio service to be idle for 3 seconds
        vTaskDelay(pdMS_TO_TICKS(3000));
        SetDeviceState(kDeviceStateUpgrading);
        board.SetPowerSaveLevel(PowerSaveLevel::PERFORMANCE);
        display->SetChatMessage("system", Lang::Strings::PLEASE_WAIT);

        bool success = assets.Download(download_url, [this, display](int progress, size_t speed) -> void {
            char buffer[32];
            snprintf(buffer, sizeof(buffer), "%d%% %uKB/s", progress, speed / 1024);
            Schedule([display, message = std::string(buffer)]() {
                display->SetChatMessage("system", message.c_str());
            });
        });

        board.SetPowerSaveLevel(PowerSaveLevel::LOW_POWER);
        vTaskDelay(pdMS_TO_TICKS(1000));

        if (!success) {
            Alert(Lang::Strings::ERROR, Lang::Strings::DOWNLOAD_ASSETS_FAILED, "circle_xmark", Lang::Sounds::OGG_EXCLAMATION);
            vTaskDelay(pdMS_TO_TICKS(2000));
            SetDeviceState(kDeviceStateActivating);
            return;
        }
    }

    // Apply assets
    assets.Apply();
    display->SetChatMessage("system", "");
    display->SetEmotion("microchip_ai");
}

void Application::CheckNewVersion() {
    const int MAX_RETRY = 10;
    int retry_count = 0;
    int retry_delay = 10; // Initial retry delay in seconds

    auto& board = Board::GetInstance();
    while (true) {
        auto display = board.GetDisplay();
        display->SetStatus(Lang::Strings::CHECKING_NEW_VERSION);

        esp_err_t err = ota_->CheckVersion();
        if (err != ESP_OK) {
            retry_count++;
            if (retry_count >= MAX_RETRY) {
                ESP_LOGE(TAG, "Too many retries, exit version check");
                return;
            }

            char error_message[128];
            snprintf(error_message, sizeof(error_message), "code=%d, url=%s", err, ota_->GetCheckVersionUrl().c_str());
            char buffer[256];
            snprintf(buffer, sizeof(buffer), Lang::Strings::CHECK_NEW_VERSION_FAILED, retry_delay, error_message);
            Alert(Lang::Strings::ERROR, buffer, "cloud_slash", Lang::Sounds::OGG_EXCLAMATION);

            ESP_LOGW(TAG, "Check new version failed, retry in %d seconds (%d/%d)", retry_delay, retry_count, MAX_RETRY);
            for (int i = 0; i < retry_delay; i++) {
                vTaskDelay(pdMS_TO_TICKS(1000));
                if (GetDeviceState() == kDeviceStateIdle) {
                    break;
                }
            }
            retry_delay *= 2; // Double the retry delay
            continue;
        }
        retry_count = 0;
        retry_delay = 10; // Reset retry delay

        if (ota_->HasNewVersion()) {
            if (UpgradeFirmware(ota_->GetFirmwareUrl(), ota_->GetFirmwareVersion())) {
                return; // This line will never be reached after reboot
            }
            // If upgrade failed, continue to normal operation
        }

        // No new version, mark the current version as valid
        ota_->MarkCurrentVersionValid();
        if (!ota_->HasActivationCode() && !ota_->HasActivationChallenge()) {
            // Exit the loop if done checking new version
            break;
        }

        display->SetStatus(Lang::Strings::ACTIVATION);
        // Activation code is shown to the user and waiting for the user to input
        if (ota_->HasActivationCode()) {
            ShowActivationCode(ota_->GetActivationCode(), ota_->GetActivationMessage());
        }

        // This will block the loop until the activation is done or timeout
        for (int i = 0; i < 10; ++i) {
            ESP_LOGI(TAG, "Activating... %d/%d", i + 1, 10);
            esp_err_t err = ota_->Activate();
            if (err == ESP_OK) {
                break;
            } else if (err == ESP_ERR_TIMEOUT) {
                vTaskDelay(pdMS_TO_TICKS(3000));
            } else {
                vTaskDelay(pdMS_TO_TICKS(10000));
            }
            if (GetDeviceState() == kDeviceStateIdle) {
                break;
            }
        }
    }
}

void Application::InitializeProtocol() {
    auto& board = Board::GetInstance();
    auto display = board.GetDisplay();
    auto codec = board.GetAudioCodec();

    display->SetStatus(Lang::Strings::LOADING_PROTOCOL);

    if (ota_->HasMqttConfig()) {
        protocol_ = std::make_unique<MqttProtocol>();
    } else if (ota_->HasWebsocketConfig()) {
        protocol_ = std::make_unique<WebsocketProtocol>();
    } else {
        ESP_LOGW(TAG, "No protocol specified in the OTA config, using MQTT");
        protocol_ = std::make_unique<MqttProtocol>();
    }

    protocol_->OnConnected([this]() {
        DismissAlert();
    });

    protocol_->OnNetworkError([this](const std::string& message) {
        last_error_message_ = message;
        xEventGroupSetBits(event_group_, MAIN_EVENT_ERROR);
    });
    
    protocol_->OnIncomingAudio([this](std::unique_ptr<AudioStreamPacket> packet) {
        // Gate on the WS-task-owned utterance flag, not the device state: the
        // Speaking transition runs via Schedule on the main task, which can be
        // blocked (WaitForPlaybackQueueEmpty during the previous utterance's
        // Listening entry) for seconds — every head-of-utterance frame in that
        // window would be dropped.
        packet->from_server = true;
        bool queued = false;
        if (utterance_open_.load()) {
            queued = audio_service_.PushPacketToDecodeQueue(std::move(packet));
        }
        if (!queued) {
            // Dropped (no open utterance, or decode queue full): the frame
            // consumed server send budget (§5), so credit it back — otherwise
            // the server's window shrinks permanently with every drop.
            Schedule([this]() {
                if (protocol_) {
                    protocol_->NotifyAudioFrameConsumed(1);
                }
            });
        }
    });

    // Protocol v2 audio flow control (§5): grant the server more send credit as
    // decoded TTS frames drain off the decode queue (or get flushed by a
    // ResetDecoder, in bulk). The hook fires on the opus codec task (or the
    // flushing task), so marshal onto the main task (via Schedule) to serialize
    // the audio_credit send with every other WS send — Protocol batches the
    // grants.
    audio_service_.OnDecodeFrameConsumed([this](int frames) {
        Schedule([this, frames]() {
            if (protocol_) {
                protocol_->NotifyAudioFrameConsumed(frames);
            }
        });
    });
    
    protocol_->OnAudioChannelOpened([this, codec, &board]() {
        utterance_open_.store(false);  // fresh session, no utterance in flight
        board.SetPowerSaveLevel(PowerSaveLevel::PERFORMANCE);
        if (protocol_->server_sample_rate() != codec->output_sample_rate()) {
            ESP_LOGW(TAG, "Server sample rate %d does not match device output sample rate %d, resampling may cause distortion",
                protocol_->server_sample_rate(), codec->output_sample_rate());
        }
    });
    
    protocol_->OnAudioChannelClosed([this, &board]() {
        utterance_open_.store(false);
        board.SetPowerSaveLevel(PowerSaveLevel::LOW_POWER);
        Schedule([this]() {
            auto display = Board::GetInstance().GetDisplay();
            display->SetChatMessage("system", "");
            SetDeviceState(kDeviceStateIdle);
        });
    });
    
    protocol_->OnIncomingJson([this, display](const cJSON* root) {
        // Protocol v2 dispatcher (docs/PROTOCOL_V2.md §4). The speaking state is
        // now driven by audio_begin/audio_end (§4.4), decoupled from captions —
        // this is the real fix for the v1 clipped-second-sentence bug.
        auto type = cJSON_GetObjectItem(root, "type");
        if (!cJSON_IsString(type)) {
            ESP_LOGW(TAG, "Incoming frame missing 'type'");
            return;
        }

        if (strcmp(type->valuestring, "transcript") == 0) {
            // ASR result (§4.3): display-only, drives the thinking emotion.
            auto text = cJSON_GetObjectItem(root, "text");
            if (cJSON_IsString(text)) {
                ESP_LOGI(TAG, ">> %s", text->valuestring);
                Schedule([display, message = std::string(text->valuestring)]() {
                    display->SetChatMessage("user", message.c_str());
                    display->SetEmotion("thinking");
                });
            }
        } else if (strcmp(type->valuestring, "display") == 0) {
            // Avatar display (§4.5). Apply emotion; honor status for the
            // tokens that map onto a device status concept. The device state
            // machine also drives SetStatus on its own transitions, so these
            // are duplicates of transitions about to happen anyway — the
            // branches are idempotent. "thinking" is deliberately unmapped
            // (no device status exists for it; the thinking *emotion*, sent
            // alongside, drives the overlay), and unknown tokens are ignored
            // rather than fed to SetStatus, whose fallback would paint the
            // raw token into the speech bubble.
            auto emotion = cJSON_GetObjectItem(root, "emotion");
            if (cJSON_IsString(emotion)) {
                Schedule([display, emotion_str = std::string(emotion->valuestring)]() {
                    display->SetEmotion(emotion_str.c_str());
                });
            }
            auto status = cJSON_GetObjectItem(root, "status");
            if (cJSON_IsString(status)) {
                const char* mapped = nullptr;
                if (strcmp(status->valuestring, "listening") == 0) {
                    mapped = Lang::Strings::LISTENING;
                } else if (strcmp(status->valuestring, "speaking") == 0) {
                    mapped = Lang::Strings::SPEAKING;
                }
                if (mapped != nullptr) {
                    Schedule([display, mapped]() {
                        display->SetStatus(mapped);
                    });
                }
            }
        } else if (strcmp(type->valuestring, "audio_begin") == 0) {
            // Server is about to stream TTS Opus frames (§4.4). Open the
            // utterance gate HERE, on the WS task, so the frames that follow
            // in this same socket stream are accepted immediately — the
            // Speaking state transition below runs via Schedule and may lag.
            utterance_open_.store(true);
            Schedule([this]() {
                aborted_ = false;
                if (GetDeviceState() != kDeviceStateSpeaking) {
                    SetDeviceState(kDeviceStateSpeaking);
                }
            });
        } else if (strcmp(type->valuestring, "audio_end") == 0) {
            // TTS stream finished normally (§4.4): leave Speaking. The gate
            // closes on the WS task — no frames follow audio_end (§4.4), and
            // anything that does arrive is a stray to drop-and-credit.
            utterance_open_.store(false);
            Schedule([this]() {
                if (GetDeviceState() == kDeviceStateSpeaking) {
                    // Privacy gate (sleep): after a farewell finishes, go back
                    // to Idle (mic off), NOT Listening — the gate would refuse
                    // Listening anyway, leaving us stuck in Speaking.
                    if (listening_mode_ == kListeningModeManualStop || privacy_gate_) {
                        SetDeviceState(kDeviceStateIdle);
                    } else {
                        SetDeviceState(kDeviceStateListening);
                    }
                }
            });
        } else if (strcmp(type->valuestring, "audio_cancel") == 0) {
            // Server cancelled the in-flight reply (barge-in confirmation,
            // §4.4): flush queued audio so nothing stale plays, then leave
            // Speaking.
            utterance_open_.store(false);
            Schedule([this]() {
                audio_service_.ResetDecoder();
                if (GetDeviceState() == kDeviceStateSpeaking) {
                    if (listening_mode_ == kListeningModeManualStop || privacy_gate_) {
                        SetDeviceState(kDeviceStateIdle);
                    } else {
                        SetDeviceState(kDeviceStateListening);
                    }
                }
            });
        } else if (strcmp(type->valuestring, "caption") == 0) {
            // Chat text for the current utterance (§4.4). Cumulative text rides
            // the non-terminal captions; the terminal one (final:true) omits
            // text and is a pure completion marker — nothing to display.
            auto text = cJSON_GetObjectItem(root, "text");
            if (cJSON_IsString(text) && text->valuestring[0] != '\0') {
                ESP_LOGI(TAG, "<< %s", text->valuestring);
                Schedule([this, display, message = std::string(text->valuestring)]() {
                    if (GetDeviceState() != kDeviceStateSpeaking) {
                        SetDeviceState(kDeviceStateSpeaking);
                    }
                    display->SetChatMessage("assistant", message.c_str());
                });
            }
        } else if (strcmp(type->valuestring, "alert") == 0) {
            // Full-screen popup (§4.6): title/message/emotion required.
            auto title = cJSON_GetObjectItem(root, "title");
            auto message = cJSON_GetObjectItem(root, "message");
            auto emotion = cJSON_GetObjectItem(root, "emotion");
            if (cJSON_IsString(title) && cJSON_IsString(message) && cJSON_IsString(emotion)) {
                // sound values are firmware-specific (§4.6): map the tokens we
                // have assets for; anything else (or absent) falls back to the
                // vibration chirp.
                std::string_view sound = Lang::Sounds::OGG_VIBRATION;
                auto sound_field = cJSON_GetObjectItem(root, "sound");
                if (cJSON_IsString(sound_field)) {
                    const char* s = sound_field->valuestring;
                    if (strcmp(s, "popup") == 0) {
                        sound = Lang::Sounds::OGG_POPUP;
                    } else if (strcmp(s, "success") == 0) {
                        sound = Lang::Sounds::OGG_SUCCESS;
                    } else if (strcmp(s, "exclamation") == 0) {
                        sound = Lang::Sounds::OGG_EXCLAMATION;
                    } else if (strcmp(s, "low_battery") == 0) {
                        sound = Lang::Sounds::OGG_LOW_BATTERY;
                    }
                }
                // Marshal off the WS receive task: Alert takes the LVGL lock
                // and PlaySound pushes with wait=true — behind a full decode
                // queue during active TTS that would block the socket reader
                // and stall every inbound frame.
                Schedule([this, t = std::string(title->valuestring),
                          m = std::string(message->valuestring),
                          e = std::string(emotion->valuestring), sound]() {
                    Alert(t.c_str(), m.c_str(), e.c_str(), sound);
                });
            } else {
                ESP_LOGW(TAG, "Alert requires title, message and emotion");
            }
        } else if (strcmp(type->valuestring, "system") == 0) {
            auto command = cJSON_GetObjectItem(root, "command");
            if (cJSON_IsString(command)) {
                ESP_LOGI(TAG, "System command: %s", command->valuestring);
                if (strcmp(command->valuestring, "reboot") == 0) {
                    // Do a reboot if user requests a OTA update
                    Schedule([this]() {
                        Reboot();
                    });
                } else {
                    ESP_LOGW(TAG, "Unknown system command: %s", command->valuestring);
                }
            }
        } else if (strcmp(type->valuestring, "tool_list") == 0) {
            // First-class tool discovery (§6.2) — bridge to the MCP registry.
            McpServer::GetInstance().HandleToolList(root);
        } else if (strcmp(type->valuestring, "tool_call") == 0) {
            // First-class tool invocation (§6.3) — bridge to the MCP registry.
            McpServer::GetInstance().HandleToolCall(root);
        } else if (strcmp(type->valuestring, "goodbye") == 0) {
            // Advisory only (§4.10): the WS close frame does the teardown.
            auto reason = cJSON_GetObjectItem(root, "reason");
            ESP_LOGI(TAG, "Goodbye: %s", cJSON_IsString(reason) ? reason->valuestring : "");
        } else if (strcmp(type->valuestring, "error") == 0) {
            // Log + degrade; never fatal (§9). Tolerate unknown codes.
            auto code = cJSON_GetObjectItem(root, "code");
            auto message = cJSON_GetObjectItem(root, "message");
            ESP_LOGW(TAG, "Server error %s: %s",
                     cJSON_IsString(code) ? code->valuestring : "?",
                     cJSON_IsString(message) ? message->valuestring : "");
        } else {
            ESP_LOGW(TAG, "Unknown message type: %s", type->valuestring);
        }
    });
    
    protocol_->Start();
}

void Application::ShowActivationCode(const std::string& code, const std::string& message) {
    // struct digit_sound {
    //     char digit;
    //     const std::string_view& sound;
    // };
    // static const std::array<digit_sound, 10> digit_sounds{{
    //     digit_sound{'0', Lang::Sounds::OGG_0},
    //     digit_sound{'1', Lang::Sounds::OGG_1}, 
    //     digit_sound{'2', Lang::Sounds::OGG_2},
    //     digit_sound{'3', Lang::Sounds::OGG_3},
    //     digit_sound{'4', Lang::Sounds::OGG_4},
    //     digit_sound{'5', Lang::Sounds::OGG_5},
    //     digit_sound{'6', Lang::Sounds::OGG_6},
    //     digit_sound{'7', Lang::Sounds::OGG_7},
    //     digit_sound{'8', Lang::Sounds::OGG_8},
    //     digit_sound{'9', Lang::Sounds::OGG_9}
    // }};

    // // This sentence uses 9KB of SRAM, so we need to wait for it to finish
    // Alert(Lang::Strings::ACTIVATION, message.c_str(), "link", Lang::Sounds::OGG_ACTIVATION);

    // for (const auto& digit : code) {
    //     auto it = std::find_if(digit_sounds.begin(), digit_sounds.end(),
    //         [digit](const digit_sound& ds) { return ds.digit == digit; });
    //     if (it != digit_sounds.end()) {
    //         audio_service_.PlaySound(it->sound);
    //     }
    // }

    auto display = Board::GetInstance().GetDisplay();
    display->SetChatMessage("system", "Please bind and set up in the mobile app.");
}

void Application::Alert(const char* status, const char* message, const char* emotion, const std::string_view& sound) {
    ESP_LOGW(TAG, "Alert [%s] %s: %s", emotion, status, message);
    auto display = Board::GetInstance().GetDisplay();
    display->SetStatus(status);
    display->SetEmotion(emotion);
    display->SetChatMessage("system", message);
    if (!sound.empty()) {
        audio_service_.PlaySound(sound);
    }
}

void Application::DismissAlert() {
    if (GetDeviceState() == kDeviceStateIdle) {
        auto display = Board::GetInstance().GetDisplay();
        display->SetStatus(Lang::Strings::STANDBY);
        display->SetEmotion("neutral");
        display->SetChatMessage("system", "");
    }
}

void Application::ToggleChatState() {
    xEventGroupSetBits(event_group_, MAIN_EVENT_TOGGLE_CHAT);
}

void Application::StartListening() {
    xEventGroupSetBits(event_group_, MAIN_EVENT_START_LISTENING);
}

void Application::StopListening() {
    xEventGroupSetBits(event_group_, MAIN_EVENT_STOP_LISTENING);
}

void Application::HandleToggleChatEvent() {
    auto state = GetDeviceState();
    
    if (state == kDeviceStateActivating) {
        SetDeviceState(kDeviceStateIdle);
        return;
    } else if (state == kDeviceStateWifiConfiguring) {
        audio_service_.EnableAudioTesting(true);
        SetDeviceState(kDeviceStateAudioTesting);
        return;
    } else if (state == kDeviceStateAudioTesting) {
        audio_service_.EnableAudioTesting(false);
        SetDeviceState(kDeviceStateWifiConfiguring);
        return;
    }

    if (!protocol_) {
        ESP_LOGE(TAG, "Protocol not initialized");
        return;
    }

    if (state == kDeviceStateIdle) {
        ListeningMode mode = GetDefaultListeningMode();
        if (!protocol_->IsAudioChannelOpened()) {
            SetDeviceState(kDeviceStateConnecting);
            // Schedule to let the state change be processed first (UI update)
            Schedule([this, mode]() {
                ContinueOpenAudioChannel(mode);
            });
            return;
        }
        SetListeningMode(mode);
    } else if (state == kDeviceStateSpeaking) {
        AbortSpeaking(kAbortReasonNone);
    } else if (state == kDeviceStateListening) {
        protocol_->CloseAudioChannel();
    }
}

void Application::ContinueOpenAudioChannel(ListeningMode mode) {
    if (GetDeviceState() != kDeviceStateConnecting) {
        return;
    }

    if (!protocol_->IsAudioChannelOpened()) {
        for (int attempt = 0; attempt < 3; attempt++) {
            if (protocol_->OpenAudioChannel()) {
                break;
            }
            if (attempt < 2) {
                vTaskDelay(pdMS_TO_TICKS(500));
                if (GetDeviceState() != kDeviceStateConnecting) {
                    return;
                }
            }
        }
        if (!protocol_->IsAudioChannelOpened()) {
            return;
        }
    }

    SetListeningMode(mode);
}

void Application::HandleStartListeningEvent() {
    auto state = GetDeviceState();
    
    if (state == kDeviceStateActivating) {
        SetDeviceState(kDeviceStateIdle);
        return;
    } else if (state == kDeviceStateWifiConfiguring) {
        audio_service_.EnableAudioTesting(true);
        SetDeviceState(kDeviceStateAudioTesting);
        return;
    }

    if (!protocol_) {
        ESP_LOGE(TAG, "Protocol not initialized");
        return;
    }
    
    if (state == kDeviceStateIdle) {
        if (!protocol_->IsAudioChannelOpened()) {
            SetDeviceState(kDeviceStateConnecting);
            // Schedule to let the state change be processed first (UI update)
            Schedule([this]() {
                ContinueOpenAudioChannel(kListeningModeManualStop);
            });
            return;
        }
        SetListeningMode(kListeningModeManualStop);
    } else if (state == kDeviceStateSpeaking) {
        AbortSpeaking(kAbortReasonNone);
        SetListeningMode(kListeningModeManualStop);
    }
}

void Application::HandleStopListeningEvent() {
    auto state = GetDeviceState();
    
    if (state == kDeviceStateAudioTesting) {
        audio_service_.EnableAudioTesting(false);
        SetDeviceState(kDeviceStateWifiConfiguring);
        return;
    } else if (state == kDeviceStateListening) {
        if (protocol_) {
            protocol_->SendStopListening();
        }
        SetDeviceState(kDeviceStateIdle);
    }
}

void Application::HandleWakeWordDetectedEvent() {
    if (!protocol_) {
        return;
    }

    auto state = GetDeviceState();
    auto wake_word = audio_service_.GetLastWakeWord();
    ESP_LOGI(TAG, "Wake word detected: %s (state: %d)", wake_word.c_str(), (int)state);

    if (state == kDeviceStateIdle) {
        audio_service_.EncodeWakeWord();
        auto wake_word = audio_service_.GetLastWakeWord();

        // Always go through the Schedule path so the state transition to
        // Connecting happens whether or not the WS audio channel is
        // already open. Stock v2.2.4 had a synchronous fast-path here
        // (when channel was already open) that skipped the state set;
        // ContinueWakeWordInvoke's state-check then early-returned and
        // wake-word silently failed. Dotty's SendEvent lazy-opens the
        // channel for telemetry from idle, so the "already open" case
        // is the common case in this firmware.
        SetDeviceState(kDeviceStateConnecting);
        Schedule([this, wake_word]() {
            ContinueWakeWordInvoke(wake_word);
        });
    } else if (state == kDeviceStateSpeaking || state == kDeviceStateListening) {
        AbortSpeaking(kAbortReasonWakeWordDetected);
        // Clear send queue to avoid sending residues to server
        while (audio_service_.PopPacketFromSendQueue());

        if (state == kDeviceStateListening) {
            protocol_->SendStartListening(GetDefaultListeningMode());
            audio_service_.ResetDecoder();
            audio_service_.PlaySound(Lang::Sounds::OGG_POPUP);
            // Re-enable wake word detection as it was stopped by the detection itself
            audio_service_.EnableWakeWordDetection(true);
        } else {
            // Play popup sound and start listening again
            play_popup_on_listening_ = true;
            SetListeningMode(GetDefaultListeningMode());
        }
    } else if (state == kDeviceStateActivating) {
        // Restart the activation check if the wake word is detected during activation
        SetDeviceState(kDeviceStateIdle);
    }
}

void Application::ContinueWakeWordInvoke(const std::string& wake_word) {
    // Check state again in case it was changed during scheduling
    if (GetDeviceState() != kDeviceStateConnecting) {
        return;
    }

    if (!protocol_->IsAudioChannelOpened()) {
        if (!protocol_->OpenAudioChannel()) {
            audio_service_.EnableWakeWordDetection(true);
            return;
        }
    }

    ESP_LOGI(TAG, "Wake word detected: %s", wake_word.c_str());
#if CONFIG_SEND_WAKE_WORD_DATA
    // Pre-roll ordering per PROTOCOL_V2 §4.2: wake FIRST — it opens the
    // server's pre-roll window — then the buffered wake-word audio, then
    // listen_start (via SetListeningMode) attaches the mode's endpointer.
    // Stock v2.2.4 sent the packets before wake; the server has no window
    // open at that point and silently discards them (§7).
    protocol_->SendWakeWordDetected(wake_word);
    while (auto packet = audio_service_.PopWakeWordPacket()) {
        protocol_->SendAudio(std::move(packet));
    }

    // Set flag to play popup sound after state changes to listening
    play_popup_on_listening_ = true;
    SetListeningMode(GetDefaultListeningMode());
#else
    // Set flag to play popup sound after state changes to listening
    // (PlaySound here would be cleared by ResetDecoder in EnableVoiceProcessing)
    play_popup_on_listening_ = true;
    SetListeningMode(GetDefaultListeningMode());
#endif
}

void Application::HandleStateChangedEvent() {
    DeviceState new_state = state_machine_.GetState();
    clock_ticks_ = 0;

    auto& board = Board::GetInstance();
    auto display = board.GetDisplay();
    auto led = board.GetLed();
    led->OnStateChanged();
    
    switch (new_state) {
        case kDeviceStateUnknown:
        case kDeviceStateIdle:
            // Privacy gate (sleep): keep the mic + wake word OFF and leave
            // the display untouched — state_manager has set the Sleepy
            // avatar + "Zzz…" bubble and a forced Idle here must not clobber
            // it. Wake is touch-or-dashboard only until the gate clears.
            if (privacy_gate_) {
                audio_service_.EnableVoiceProcessing(false);
                audio_service_.EnableWakeWordDetection(false);
                break;
            }
            display->SetStatus(Lang::Strings::STANDBY);
            display->ClearChatMessages();  // Clear messages first
            display->SetEmotion("neutral"); // Then set emotion (wechat mode checks child count)
            audio_service_.EnableVoiceProcessing(false);
            audio_service_.EnableWakeWordDetection(true);
            break;
        case kDeviceStateConnecting:
            display->SetStatus(Lang::Strings::CONNECTING);
            display->SetEmotion("neutral");
            display->SetChatMessage("system", "");
            break;
        case kDeviceStateListening:
            display->SetStatus(Lang::Strings::LISTENING);
            display->SetEmotion("neutral");

            // Make sure the audio processor is running
            if (play_popup_on_listening_ || !audio_service_.IsAudioProcessorRunning()) {
                // For auto mode, wait for playback queue to be empty before enabling voice processing
                // This prevents audio truncation when STOP arrives late due to network jitter
                if (listening_mode_ == kListeningModeAutoStop) {
                    audio_service_.WaitForPlaybackQueueEmpty();
                }
                
                // Send the start listening command
                protocol_->SendStartListening(listening_mode_);
                audio_service_.EnableVoiceProcessing(true);
            }

#ifdef CONFIG_WAKE_WORD_DETECTION_IN_LISTENING
            // Enable wake word detection in listening mode (configured via Kconfig)
            audio_service_.EnableWakeWordDetection(audio_service_.IsAfeWakeWord());
#else
            // Disable wake word detection in listening mode
            audio_service_.EnableWakeWordDetection(false);
#endif
            
            // Play popup sound after ResetDecoder (in EnableVoiceProcessing) has been called
            if (play_popup_on_listening_) {
                play_popup_on_listening_ = false;
                audio_service_.PlaySound(Lang::Sounds::OGG_POPUP);
            }
            break;
        case kDeviceStateSpeaking:
            display->SetStatus(Lang::Strings::SPEAKING);

            if (listening_mode_ != kListeningModeRealtime) {
                audio_service_.EnableVoiceProcessing(false);
#ifdef CONFIG_WAKE_WORD_DETECTION_IN_SPEAKING
                // Barge-in (opt-in via Kconfig): only AFE wake word can be
                // detected in speaking mode — but keep it OFF while the privacy
                // gate is on (a gated farewell is allowed to play, but the
                // mic/wake stay disabled). OFF by default: on this no-AEC,
                // CPU-limited board the wakenet+AEC task starves audio playback
                // and cuts the TTS, and the wake word isn't reliably heard over
                // the bot's own voice anyway.
                audio_service_.EnableWakeWordDetection(!privacy_gate_ && audio_service_.IsAfeWakeWord());
#else
                audio_service_.EnableWakeWordDetection(false);
#endif
            }
            audio_service_.ResetDecoder();
            break;
        case kDeviceStateWifiConfiguring:
            audio_service_.EnableVoiceProcessing(false);
            audio_service_.EnableWakeWordDetection(false);
            break;
        default:
            // Do nothing
            break;
    }
}

void Application::Schedule(std::function<void()>&& callback) {
    {
        std::lock_guard<std::mutex> lock(mutex_);
        main_tasks_.push_back(std::move(callback));
    }
    xEventGroupSetBits(event_group_, MAIN_EVENT_SCHEDULE);
}

void Application::AbortSpeaking(AbortReason reason) {
    ESP_LOGI(TAG, "Abort speaking");
    aborted_ = true;
    if (protocol_) {
        protocol_->SendAbortSpeaking(reason);
    }
}

void Application::SetListeningMode(ListeningMode mode) {
    listening_mode_ = mode;
    SetDeviceState(kDeviceStateListening);
}

ListeningMode Application::GetDefaultListeningMode() const {
    return aec_mode_ == kAecOff ? kListeningModeAutoStop : kListeningModeRealtime;
}

void Application::Reboot() {
    ESP_LOGI(TAG, "Rebooting...");
    // Disconnect the audio channel
    if (protocol_ && protocol_->IsAudioChannelOpened()) {
        protocol_->CloseAudioChannel();
    }
    protocol_.reset();
    audio_service_.Stop();

    vTaskDelay(pdMS_TO_TICKS(1000));
    esp_restart();
}

bool Application::UpgradeFirmware(const std::string& url, const std::string& version) {
    auto& board = Board::GetInstance();
    auto display = board.GetDisplay();

    std::string upgrade_url = url;
    std::string version_info = version.empty() ? "(Manual upgrade)" : version;

    // Close audio channel if it's open
    if (protocol_ && protocol_->IsAudioChannelOpened()) {
        ESP_LOGI(TAG, "Closing audio channel before firmware upgrade");
        protocol_->CloseAudioChannel();
    }
    ESP_LOGI(TAG, "Starting firmware upgrade from URL: %s", upgrade_url.c_str());

    Alert(Lang::Strings::OTA_UPGRADE, Lang::Strings::UPGRADING, "download", Lang::Sounds::OGG_UPGRADE);
    vTaskDelay(pdMS_TO_TICKS(3000));

    SetDeviceState(kDeviceStateUpgrading);

    std::string message = std::string(Lang::Strings::NEW_VERSION) + version_info;
    display->SetChatMessage("system", message.c_str());

    board.SetPowerSaveLevel(PowerSaveLevel::PERFORMANCE);
    audio_service_.Stop();
    vTaskDelay(pdMS_TO_TICKS(1000));

    bool upgrade_success = Ota::Upgrade(upgrade_url, [this, display](int progress, size_t speed) {
        char buffer[32];
        snprintf(buffer, sizeof(buffer), "%d%% %uKB/s", progress, speed / 1024);
        Schedule([display, message = std::string(buffer)]() {
            display->SetChatMessage("system", message.c_str());
        });
    });

    if (!upgrade_success) {
        // Upgrade failed, restart audio service and continue running
        ESP_LOGE(TAG, "Firmware upgrade failed, restarting audio service and continuing operation...");
        audio_service_.Start(); // Restart audio service
        board.SetPowerSaveLevel(PowerSaveLevel::LOW_POWER); // Restore power save level
        Alert(Lang::Strings::ERROR, Lang::Strings::UPGRADE_FAILED, "circle_xmark", Lang::Sounds::OGG_EXCLAMATION);
        vTaskDelay(pdMS_TO_TICKS(3000));
        return false;
    } else {
        // Upgrade success, reboot immediately
        ESP_LOGI(TAG, "Firmware upgrade successful, rebooting...");
        display->SetChatMessage("system", "Upgrade successful, rebooting...");
        vTaskDelay(pdMS_TO_TICKS(1000)); // Brief pause to show message
        Reboot();
        return true;
    }
}

void Application::WakeWordInvoke(const std::string& wake_word) {
    if (!protocol_) {
        return;
    }

    auto state = GetDeviceState();
    
    if (state == kDeviceStateIdle) {
        audio_service_.EncodeWakeWord();

        // Always go through Connecting + Schedule, exactly like the AFE wake
        // path (HandleWakeWordDetectedEvent): stock v2.2.4's synchronous
        // fast-path for an already-open channel skipped the state set, so
        // ContinueWakeWordInvoke's Connecting-check early-returned and the
        // invoke silently no-oped — and since SendEvent lazy-opens the channel
        // from idle, "already open" is the common case here. This is what
        // killed the walk-up face greeting and the head-pet hold-to-listen.
        // Scheduling also moves the blocking pre-roll/OpenAudioChannel work
        // off the caller's task (face/pet invokes arrive on the stackchan
        // update task, which holds the LVGL lock).
        SetDeviceState(kDeviceStateConnecting);
        Schedule([this, wake_word]() {
            ContinueWakeWordInvoke(wake_word);
        });
    } else if (state == kDeviceStateSpeaking) {
        Schedule([this]() {
            AbortSpeaking(kAbortReasonNone);
        });
    } else if (state == kDeviceStateListening) {   
        Schedule([this]() {
            if (protocol_) {
                protocol_->CloseAudioChannel();
            }
        });
    }
}

bool Application::CanEnterSleepMode() {
    if (GetDeviceState() != kDeviceStateIdle) {
        return false;
    }

    if (protocol_ && protocol_->IsAudioChannelOpened()) {
        return false;
    }

    if (!audio_service_.IsIdle()) {
        return false;
    }

    // Now it is safe to enter sleep mode
    return true;
}

void Application::SendToolMessage(const std::string& frame) {
    // Always schedule to run in main task for thread safety
    Schedule([this, frame = std::move(frame)]() {
        if (protocol_) {
            protocol_->SendToolMessage(frame);
        }
    });
}

// Dotty: ambient perception event emit. Thread-safe — Schedule pushes
// the actual SendEvent onto the main task so callers (face_tracking
// modifier on Core 0, sound localizer task) don't need to know about
// the protocol's threading model.
//
// Lazy-opens the WS audio channel so events from idle state (no
// active conversation) still reach the server. xiaozhi WS lifecycle
// is otherwise session-scoped — without this, perception events
// queued at idle would silently fail (websocket_ == nullptr).
void Application::SendEvent(const std::string& name, const std::string& data_json) {
    Schedule([this, name, data_json]() {
        if (!protocol_) return;
        if (!protocol_->IsAudioChannelOpened()) {
            if (!protocol_->OpenAudioChannel()) {
                ESP_LOGW(TAG, "SendEvent: failed to open WS for %s", name.c_str());
                return;
            }
        }
        protocol_->SendEvent(name, data_json);
    });
}

void Application::SetAecMode(AecMode mode) {
    aec_mode_ = mode;
    Schedule([this]() {
        auto& board = Board::GetInstance();
        auto display = board.GetDisplay();
        switch (aec_mode_) {
        case kAecOff:
            audio_service_.EnableDeviceAec(false);
            display->ShowNotification(Lang::Strings::RTC_MODE_OFF);
            break;
        case kAecOnServerSide:
            audio_service_.EnableDeviceAec(false);
            display->ShowNotification(Lang::Strings::RTC_MODE_ON);
            break;
        case kAecOnDeviceSide:
            audio_service_.EnableDeviceAec(true);
            display->ShowNotification(Lang::Strings::RTC_MODE_ON);
            break;
        }

        // If the AEC mode is changed, close the audio channel
        if (protocol_ && protocol_->IsAudioChannelOpened()) {
            protocol_->CloseAudioChannel();
        }
    });
}

void Application::PlaySound(const std::string_view& sound) {
    audio_service_.PlaySound(sound);
}

void Application::ResetProtocol() {
    Schedule([this]() {
        // Close audio channel if opened
        if (protocol_ && protocol_->IsAudioChannelOpened()) {
            protocol_->CloseAudioChannel();
        }
        // Reset protocol
        protocol_.reset();
    });
}

