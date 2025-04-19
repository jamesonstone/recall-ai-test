## Technical Assessment 1: Video Recompositor

At Recall, we process millions of hours of video every month. As an infrastructure company we are focused on minimizing the cost of our media processing pipelines.
One of the core parts of our pipeline is our video compositor, responsible for aggregating multiple live video streams into a single video.

In this challenge, you will be implementing a similar mechanism, but with a twist. Instead of aggregating multiple live video streams, you will be provided with
a single video file, in a raw, frame-by-frame format, and you will be asked to recomposite the video with a configurable set of rules, and produce an output video file, in the same format.

Hidden in the output video file will be a secret message.

### Raw Video Format

Input and output videos format are provided in the following format:

```
[width: 4 bytes]
[height: 4 bytes]
[total number of frames (n): 4 bytes]
[frame 1]
[frame 2]
...
[frame n]
```

All integers are encoded in big-endian format.

### Frame Format

Each frame is packed with pixels encoded as a 24-bit Blue-Red-Green value, with 8 bits per channel.
Pixel order is defined as (0, 0), (1, 0)... where (x, y) is the pixel at position x, y in the frame.

```
BRGBRGBRG...
```

### Rules

The recomposition rules are provided as a JSON configuration file in the following format:

```json
{
    "size": [1920, 1080], // [width, height]
    "rects": [
        {
            "src": [0, 0, 100, 100], // [x, y, width, height]
            "dest": [20, 30], // [x, y]
            "alpha": 1.0,
            "z": 1
        },
        {
            "src": [100, 0, 100, 100], // [x, y, width, height]
            "dest": [120, 130], // [x, y]
            "alpha": 0.5,
            "z": 2
        },
        ...
    ]
}
```

Where:

- `src` is the source rectangle to copy from the input video
- `dest` is the destination rectangle to copy to the output video
- `alpha` is the opacity of composited rectangle
- `z` is the z-index which defines the order in which the rectangles are composited

## Input

The input video file is provided in the `input` directory, and the recomposition rules are provided in the `rules.json` file.

## Test

In order to test the correctness of your implementation, you can use the `test` directory, which contains sample inputs and the expected output.

## Your Task

Your goal is two-fold:

1. Find the secret message, and submit it to us.
2. Optimize your implementation, specifically, you will be assessed on the following _efficiency score_ across a range of benchmarks:
    - Efficiency Score: `Average Throughput (FPS) / Peak Memory Usage (MB)`

You will _not_ be assessed on the technology you choose to implement your solution but we suggest you consider the previous criteria carefully when choosing runtimes or libraries.
While not a primary metric, we will consider the readability of your code, and the overall quality of the solution.

Please submit your solution as a zip file containing your source code, and a README file with instructions on how to run your solution.
Please also include the secret message and the efficiency score of your solution on the large input.

## Constraints

- The solution will be tested on a `c7i.2xlarge` EC2 instance running debian bookworm. Please ensure your solution runs on this environment.
- The input and output files stored in `ramfs`, so disk I/O will not be a bottleneck for your solution.

## FAQ

1. Do I need to handle the compression/decompression of the video files?
   - No, the files are provided in a compressed format because they are large when uncompressed. Your solution should read and write the files in an uncompressed format.

2. How is the solution measured?
   - We will measure the average throughput (FPS) and peak memory usage (MB) using the `time -v` command.

3. Can I use external libraries?
   - Yes, you can use any libraries you want although we will caution you that the problem has been designed to make typical libraries challenging to use or inefficient.
