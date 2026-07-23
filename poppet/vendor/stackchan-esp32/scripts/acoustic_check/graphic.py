import sys
import numpy as np
import asyncio
import wave
from collections import deque
import qasync

import matplotlib
matplotlib.use('qtagg')  

from matplotlib.backends.backend_qtagg import FigureCanvasQTAgg as FigureCanvas
from matplotlib.backends.backend_qtagg import NavigationToolbar2QT as NavigationToolbar  # noqa: F401
from matplotlib.figure import Figure

from PyQt6.QtWidgets import (QApplication, QMainWindow, QVBoxLayout, QWidget, 
                             QHBoxLayout, QLineEdit, QPushButton, QLabel, QTextEdit)
from PyQt6.QtCore import QTimer

# Import the decoder
from demod import RealTimeAFSKDecoder


class UDPServerProtocol(asyncio.DatagramProtocol):
    """UDP server protocol class"""
    def __init__(self, data_queue):
        self.client_address = None
        self.data_queue: deque = data_queue

    def connection_made(self, transport):
        self.transport = transport
        
    def datagram_received(self, data, addr):
        # If there is no client address yet, record the first client that connects
        if self.client_address is None:
            self.client_address = addr
            print(f"Accepted connection from {addr}")

        # Only process data from the recorded client
        if addr == self.client_address:
            # Add the received audio data to the queue
            self.data_queue.extend(data)
        else:
            print(f"Ignoring data from unknown address {addr}")


class MatplotlibWidget(QWidget):
    def __init__(self, parent=None):
        super().__init__(parent)

        # Create the Matplotlib Figure object
        self.figure = Figure()

        # Create the FigureCanvas object, which is the QWidget container for the Figure
        self.canvas = FigureCanvas(self.figure)

        # Create the Matplotlib navigation toolbar
        # self.toolbar = NavigationToolbar(self.canvas, self)
        self.toolbar = None

        # Create the layout
        layout = QVBoxLayout()
        layout.addWidget(self.toolbar)
        layout.addWidget(self.canvas)
        self.setLayout(layout)

        # Initialize the audio data parameters
        self.freq = 16000  # Sample rate
        self.time_window = 20  # Display time window
        self.wave_data = deque(maxlen=self.freq * self.time_window * 2) # Buffer queue, used to dispatch computation/plotting
        self.signals = deque(maxlen=self.freq * self.time_window)  # Deque storing the signal data

        # Create the canvas containing two subplots
        self.ax1 = self.figure.add_subplot(2, 1, 1)
        self.ax2 = self.figure.add_subplot(2, 1, 2)

        # Time-domain subplot
        self.ax1.set_title('Real-time Audio Waveform')
        self.ax1.set_xlabel('Sample Index')
        self.ax1.set_ylabel('Amplitude')
        self.line_time, = self.ax1.plot([], [])
        self.ax1.grid(True, alpha=0.3)
        
        # Frequency-domain subplot
        self.ax2.set_title('Real-time Frequency Spectrum')
        self.ax2.set_xlabel('Frequency (Hz)')
        self.ax2.set_ylabel('Magnitude')
        self.line_freq, = self.ax2.plot([], [])
        self.ax2.grid(True, alpha=0.3)
        
        self.figure.tight_layout()

        # Timer used to update the chart
        self.timer = QTimer(self)
        self.timer.setInterval(100)  # Update every 100 milliseconds
        self.timer.timeout.connect(self.update_plot)

        # Initialize the AFSK decoder
        self.decoder = RealTimeAFSKDecoder(
            f_sample=self.freq,
            mark_freq=1800,
            space_freq=1500,
            bitrate=100,
            s_goertzel=9,
            threshold=0.5
        )
        
        # Decoding result callback
        self.decode_callback = None

    def start_plotting(self):
        """Start plotting"""
        self.timer.start()

    def stop_plotting(self):
        """Stop plotting"""
        self.timer.stop()

    def update_plot(self):
        """Update the plot data"""
        if len(self.wave_data) >= 2:
            # Perform real-time decoding
            # Get the latest audio data for decoding
            even = len(self.wave_data) // 2 * 2
            print(f"length of wave_data: {len(self.wave_data)}")
            drained = [self.wave_data.popleft() for _ in range(even)]
            signal = np.frombuffer(bytearray(drained), dtype='<i2') / 32768
            decoded_text_new = self.decoder.process_audio(signal) # Process the new signal, return the full decoded text
            if decoded_text_new and self.decode_callback:
                self.decode_callback(decoded_text_new)
            self.signals.extend(signal.tolist())  # Add the waveform data to the plot data

        if len(self.signals) > 0:
            # Only display the most recent segment of data to avoid an overly dense chart
            signal = np.array(self.signals)
            max_samples = min(len(signal), self.freq * self.time_window)
            if len(signal) > max_samples:
                signal = signal[-max_samples:]
            
            # Update the time-domain plot
            x = np.arange(len(signal))
            self.line_time.set_data(x, signal)

            # Automatically adjust the time-domain axis range
            if len(signal) > 0:
                self.ax1.set_xlim(0, len(signal))
                y_min, y_max = np.min(signal), np.max(signal)
                if y_min != y_max:
                    margin = (y_max - y_min) * 0.1
                    self.ax1.set_ylim(y_min - margin, y_max + margin)
                else:
                    self.ax1.set_ylim(-1, 1)
            
            # Compute the spectrum (short-time discrete Fourier transform)
            if len(signal) > 1:
                # Compute the FFT
                fft_signal = np.abs(np.fft.fft(signal))
                frequencies = np.fft.fftfreq(len(signal), 1/self.freq)

                # Take only the positive-frequency part
                positive_freq_idx = frequencies >= 0
                freq_positive = frequencies[positive_freq_idx]
                fft_positive = fft_signal[positive_freq_idx]

                # Update the frequency-domain plot
                self.line_freq.set_data(freq_positive, fft_positive)

                # Automatically adjust the frequency-domain axis range
                if len(fft_positive) > 0:
                    # Limit the frequency display range to 0-4000Hz to avoid excessive density
                    max_freq_show = min(4000, self.freq // 2)
                    freq_mask = freq_positive <= max_freq_show
                    if np.any(freq_mask):
                        self.ax2.set_xlim(0, max_freq_show)
                        fft_masked = fft_positive[freq_mask]
                        if len(fft_masked) > 0:
                            fft_max = np.max(fft_masked)
                            if fft_max > 0:
                                self.ax2.set_ylim(0, fft_max * 1.1)
                            else:
                                self.ax2.set_ylim(0, 1)
            
            self.canvas.draw()


class MainWindow(QMainWindow):
    def __init__(self):
        super().__init__()
        self.setWindowTitle("Acoustic Check")
        self.setGeometry(100, 100, 1000, 800)

        # Main window widget
        main_widget = QWidget()
        self.setCentralWidget(main_widget)

        # Main layout
        main_layout = QVBoxLayout(main_widget)

        # Plotting area
        self.matplotlib_widget = MatplotlibWidget()
        main_layout.addWidget(self.matplotlib_widget)

        # Control panel
        control_panel = QWidget()
        control_layout = QHBoxLayout(control_panel)

        # Listen address and port input
        control_layout.addWidget(QLabel("Listen address:"))
        self.address_input = QLineEdit("0.0.0.0")
        self.address_input.setFixedWidth(120)
        control_layout.addWidget(self.address_input)

        control_layout.addWidget(QLabel("Port:"))
        self.port_input = QLineEdit("8000")
        self.port_input.setFixedWidth(80)
        control_layout.addWidget(self.port_input)

        # Listen button
        self.listen_button = QPushButton("Start listening")
        self.listen_button.clicked.connect(self.toggle_listening)
        control_layout.addWidget(self.listen_button)

        # Status label
        self.status_label = QLabel("Status: Not connected")
        control_layout.addWidget(self.status_label)

        # Data statistics label
        self.data_label = QLabel("Received data: 0 bytes")
        control_layout.addWidget(self.data_label)

        # Save button
        self.save_button = QPushButton("Save audio")
        self.save_button.clicked.connect(self.save_audio)
        self.save_button.setEnabled(False)
        control_layout.addWidget(self.save_button)

        control_layout.addStretch()  # Add flexible space
        
        main_layout.addWidget(control_panel)
        
        # Decode display area
        decode_panel = QWidget()
        decode_layout = QVBoxLayout(decode_panel)

        # Decode title
        decode_title = QLabel("Real-time AFSK decoding results:")
        decode_title.setStyleSheet("font-weight: bold; font-size: 14px;")
        decode_layout.addWidget(decode_title)

        # Decoded text display
        self.decode_text = QTextEdit()
        self.decode_text.setMaximumHeight(150)
        self.decode_text.setReadOnly(True)
        self.decode_text.setStyleSheet("font-family: 'Courier New', monospace; font-size: 12px;")
        decode_layout.addWidget(self.decode_text)
        
        # Decode control buttons
        decode_control_layout = QHBoxLayout()

        # Clear button
        self.clear_decode_button = QPushButton("Clear decoding")
        self.clear_decode_button.clicked.connect(self.clear_decode_text)
        decode_control_layout.addWidget(self.clear_decode_button)

        # Decode statistics label
        self.decode_stats_label = QLabel("Decode stats: 0 bits, 0 chars")
        decode_control_layout.addWidget(self.decode_stats_label)
        
        decode_control_layout.addStretch()
        decode_layout.addLayout(decode_control_layout)
        
        main_layout.addWidget(decode_panel)
        
        # Set the decode callback
        self.matplotlib_widget.decode_callback = self.on_decode_text

        # UDP-related attributes
        self.udp_transport = None
        self.is_listening = False

        # Data statistics timer
        self.stats_timer = QTimer(self)
        self.stats_timer.setInterval(1000)  # Update statistics once per second
        self.stats_timer.timeout.connect(self.update_stats)

    def on_decode_text(self, new_text: str):
        """Decoded text callback"""
        if new_text:
            # Append the newly decoded text
            current_text = self.decode_text.toPlainText()
            updated_text = current_text + new_text

            # Limit the text length, keeping the most recent 1000 characters
            if len(updated_text) > 1000:
                updated_text = updated_text[-1000:]

            self.decode_text.setPlainText(updated_text)

            # Scroll to the bottom
            cursor = self.decode_text.textCursor()
            cursor.movePosition(cursor.MoveOperation.End)
            self.decode_text.setTextCursor(cursor)
            
    def clear_decode_text(self):
        """Clear the decoded text"""
        self.decode_text.clear()
        if hasattr(self.matplotlib_widget, 'decoder'):
            self.matplotlib_widget.decoder.clear()
        self.decode_stats_label.setText("Decode stats: 0 bits, 0 chars")

    def update_decode_stats(self):
        """Update the decode statistics"""
        if hasattr(self.matplotlib_widget, 'decoder'):
            stats = self.matplotlib_widget.decoder.get_stats()
            stats_text = (
                f"Prelude: {stats['prelude_bits']} , received {stats['total_chars']} chars, "
                f"buffer: {stats['buffer_bits']} bits, state: {stats['state']}"
            )
            self.decode_stats_label.setText(stats_text)
        
    def toggle_listening(self):
        """Toggle the listening state"""
        if not self.is_listening:
            self.start_listening()
        else:
            self.stop_listening()

    async def start_listening_async(self):
        """Start UDP listening asynchronously"""
        try:
            address = self.address_input.text().strip()
            port = int(self.port_input.text().strip())
            
            loop = asyncio.get_running_loop()
            self.udp_transport, protocol = await loop.create_datagram_endpoint(
                lambda: UDPServerProtocol(self.matplotlib_widget.wave_data),
                local_addr=(address, port)
            )
            
            self.status_label.setText(f"Status: Listening ({address}:{port})")
            print(f"UDP server started, listening on {address}:{port}")

        except Exception as e:
            self.status_label.setText(f"Status: Startup failed - {str(e)}")
            print(f"UDP server startup failed: {e}")
            self.is_listening = False
            self.listen_button.setText("Start listening")
            self.address_input.setEnabled(True)
            self.port_input.setEnabled(True)

    def start_listening(self):
        """Start listening"""
        try:
            int(self.port_input.text().strip())  # Validate the port number format
        except ValueError:
            self.status_label.setText("Status: Port number must be a number")
            return
            
        self.is_listening = True
        self.listen_button.setText("Stop listening")
        self.address_input.setEnabled(False)
        self.port_input.setEnabled(False)
        self.save_button.setEnabled(True)

        # Clear the data queue
        self.matplotlib_widget.wave_data.clear()

        # Start plotting and statistics updates
        self.matplotlib_widget.start_plotting()
        self.stats_timer.start()

        # Start the UDP server asynchronously
        loop = asyncio.get_event_loop()
        loop.create_task(self.start_listening_async())

    def stop_listening(self):
        """Stop listening"""
        self.is_listening = False
        self.listen_button.setText("Start listening")
        self.address_input.setEnabled(True)
        self.port_input.setEnabled(True)

        # Stop the UDP server
        if self.udp_transport:
            self.udp_transport.close()
            self.udp_transport = None

        # Stop plotting and statistics updates
        self.matplotlib_widget.stop_plotting()
        self.matplotlib_widget.wave_data.clear()
        self.stats_timer.stop()

        self.status_label.setText("Status: Stopped")

    def update_stats(self):
        """Update the data statistics"""
        data_size = len(self.matplotlib_widget.signals)
        self.data_label.setText(f"Received data: {data_size} samples")

        # Update the decode statistics
        self.update_decode_stats()
        
    def save_audio(self):
        """Save the audio data"""
        if len(self.matplotlib_widget.signals) > 0:
            try:
                signal_data = np.array(self.matplotlib_widget.signals)

                # Save as a WAV file
                with wave.open("received_audio.wav", "wb") as wf:
                    wf.setnchannels(1)  # Mono
                    wf.setsampwidth(2)  # Sample width of 2 bytes
                    wf.setframerate(self.matplotlib_widget.freq)  # Set the sample rate
                    wf.writeframes(signal_data.tobytes())  # Write the data

                self.status_label.setText("Status: Audio saved as received_audio.wav")
                print("Audio saved as received_audio.wav")

            except Exception as e:
                self.status_label.setText(f"Status: Save failed - {str(e)}")
                print(f"Failed to save audio: {e}")
        else:
            self.status_label.setText("Status: Not enough data to save")


async def main():
    """Asynchronous main function"""
    app = QApplication(sys.argv)

    # Set up the asynchronous event loop
    loop = qasync.QEventLoop(app)
    asyncio.set_event_loop(loop)

    window = MainWindow()
    window.show()

    try:
        with loop:
            await loop.run_forever()
    except KeyboardInterrupt:
        print("Program interrupted by user")
    finally:
        # Ensure resources are cleaned up
        if window.udp_transport:
            window.udp_transport.close()