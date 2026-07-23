# Acoustic Test
This GUI is used to test converting the `pcm` that the device sends back over `udp` into the time/frequency domain. It can save a window's worth of audio, which is useful for assessing the noise frequency distribution and testing the accuracy of transmitting ASCII over acoustic signals.

Firmware testing requires enabling `USE_AUDIO_DEBUGGER` and setting `AUDIO_DEBUG_UDP_SERVER` to this machine's address.
Acoustic `demod` can be driven via `sonic_wifi_config.html` or by uploading to `PinMe`'s [acoustic Wi-Fi provisioning](https://iqf7jnhi.pinit.eth.limo) to produce the acoustic test.

# Acoustic Decoding Test Records

> `✓` means decoding succeeds directly from the raw PCM signal received on I2S DIN, `△` means stable decoding requires noise reduction or extra steps, `X` means results are poor even after noise reduction (partial decoding may be possible but is very unstable).
> Some ADCs require finer noise-reduction tuning during the I2C configuration stage; since devices are not interchangeable, testing for now follows only the config provided in `boards`.

| Device | ADC | MIC | Result | Notes |
| ---- | ---- | --- | --- | ---- |
| bread-compact | INMP441 | Integrated MEMS MIC | ✓ |
| atk-dnesp32s3-box | ES8311 | | ✓ |
| magiclick-2p5 | ES8311 | | ✓ |
| lichuang-dev  | ES7210 | | △ | INPUT_REFERENCE must be turned off during testing
| kevin-box-2 | ES7210 | | △ | INPUT_REFERENCE must be turned off during testing
| m5stack-core-s3 | ES7210 | | △ | INPUT_REFERENCE must be turned off during testing
| xmini-c3 | ES8311 | | △ | Noise reduction required
| atoms3r-echo-base | ES8311 | | △ | Noise reduction required
| atk-dnesp32s3-box0 | ES8311 | | X | Can receive and decode, but the packet loss rate is very high
| movecall-moji-esp32s3 | ES8311 | | X | Can receive and decode, but the packet loss rate is very high