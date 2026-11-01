# Project landing page

This static page is an optional project showcase, separate from the Go services.
Delete the `landing/` directory to remove it; it does not participate in builds or
deployment.

To preview it locally, from the repository root run:

```sh
python3 -m http.server 4173 --directory landing
```

Then open <http://127.0.0.1:4173/>. It uses HTML, CSS and a small JavaScript file;
there is no build step or package manager. Google Fonts are optional. The terminal
is a static illustration of the sample workflow, not a live cluster view.

The style takes broad cues from [Factory's landing page](https://factory.com/): a
confident oversized headline, strong type hierarchy, and capability sections.
The copy, visual identity, diagrams and layout are original to this project.
