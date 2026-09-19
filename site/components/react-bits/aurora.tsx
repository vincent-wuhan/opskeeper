'use client';

import { useEffect, useRef } from 'react';
import {
  Renderer,
  Program,
  Mesh,
  Triangle,
  Color,
} from 'ogl';

/**
 * React Bits "Aurora" — animated WebGL aurora background.
 * Drop-in copy-paste variant adapted to this site's Tailwind palette
 * (accent green #10a37f on near-black ink). Sits behind hero content at -z.
 */
export default function Aurora({
  className = '',
  colorStops = [0.064, 0.639, 0.498],
  amplitude = 1.0,
  blend = 0.5,
  speed = 0.6,
}: {
  className?: string;
  colorStops?: number[];
  amplitude?: number;
  blend?: number;
  speed?: number;
}) {
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const probe = document.createElement('canvas');
    const probeContext = probe.getContext('webgl2') || probe.getContext('webgl');
    if (!probeContext) return;

    let renderer: Renderer;
    try {
      renderer = new Renderer({
        alpha: true,
        antialias: true,
        dpr: Math.min(window.devicePixelRatio || 1, 2),
      });
    } catch {
      return;
    }
    const gl = renderer.gl;
    if (!gl) return;
    gl.clearColor(0, 0, 0, 0);

    const vertex = /* glsl */ `
      #ifdef GL_ES
      precision highp float;
      #endif
      attribute vec3 position;
      varying vec2 vUv;
      void main() {
        vUv = position.xy * 0.5 + 0.5;
        gl_Position = vec4(position, 1.0);
      }
    `;

    // React Bits Aurora fragment shader (adapted uniforms/palette)
    const fragment = /* glsl */ `
      #ifdef GL_ES
      precision highp float;
      #endif
      uniform float uTime;
      uniform vec2 uResolution;
      uniform vec3 uColor1;
      uniform vec3 uColor2;
      uniform vec3 uColor3;
      uniform float uAmplitude;
      uniform float uBlend;
      varying vec2 vUv;

      float rand(vec2 co) {
        return fract(sin(dot(co, vec2(12.9898, 78.233))) * 43758.5453);
      }

      float noise(vec2 p) {
        vec2 i = floor(p);
        vec2 f = fract(p);
        vec2 u = f * f * (3.0 - 2.0 * f);
        float a = rand(i);
        float b = rand(i + vec2(1.0, 0.0));
        float c = rand(i + vec2(0.0, 1.0));
        float d = rand(i + vec2(1.0, 1.0));
        return mix(mix(a, b, u.x), mix(c, d, u.x), u.y);
      }

      float fbm(vec2 p) {
        float v = 0.0;
        float a = 0.5;
        for (int i = 0; i < 5; i++) {
          v += a * noise(p);
          p = p * 2.0 + vec2(100.0);
          a *= 0.5;
        }
        return v;
      }

      float elevation(float x, float t, float freq) {
        float n = fbm(vec2(x * freq, t * 0.35));
        return uAmplitude * n * 0.5;
      }

      vec3 layerColor(float y, float band, vec3 c) {
        float d = y - band;
        float intensity = exp(-abs(d) * 2.2);
        return c * intensity;
      }

      void main() {
        vec2 uv = vUv;
        float t = uTime * 0.25;

        float e1 = elevation(uv.x, t, 2.0);
        float e2 = elevation(uv.x + 10.0, t * 1.3, 3.0);
        float e3 = elevation(uv.x + 20.0, t * 0.7, 4.0);

        vec3 col = vec3(0.0);
        col += layerColor(uv.y, 0.35 + e1 * 0.25, uColor1) * 1.0;
        col += layerColor(uv.y, 0.5 + e2 * 0.22, uColor2) * 0.8;
        col += layerColor(uv.y, 0.62 + e3 * 0.2, uColor3) * 0.6;

        // fade out top and bottom, stronger glow near the top of the page
        float fade = smoothstep(0.0, 0.25, uv.y) * (1.0 - smoothstep(0.75, 1.0, uv.y));
        col *= mix(0.35, 1.0, fade);
        col *= uBlend;

        // subtle horizontal falloff so the edges melt into the page background
        float edge = smoothstep(0.0, 0.18, uv.x) * (1.0 - smoothstep(0.82, 1.0, uv.x));
        col *= mix(0.55, 1.0, edge);

        gl_FragColor = vec4(col, 1.0);
      }
    `;

    const geometry = new Triangle(gl);
    const program = new Program(gl, {
      vertex,
      fragment,
      uniforms: {
        uTime: { value: 0 },
        uResolution: { value: new Color(0, 0, 0) },
        uColor1: { value: new Color(colorStops[0] ?? 0.064, colorStops[1] ?? 0.639, colorStops[2] ?? 0.498) },
        uColor2: { value: new Color(0.2, 0.8, 0.7) },
        uColor3: { value: new Color(0.1, 0.3, 0.9) },
        uAmplitude: { value: amplitude },
        uBlend: { value: blend },
      },
    });

    const mesh = new Mesh(gl, { geometry, program });

    function resize() {
      const rect = container!.getBoundingClientRect();
      renderer.setSize(rect.width, rect.height);
      program.uniforms.uResolution.value.set(gl.canvas.width, gl.canvas.height, 1);
    }
    window.addEventListener('resize', resize);
    resize();

    let raf = 0;
    let visible = true;
    const io = new IntersectionObserver((entries) => {
      visible = entries[0]?.isIntersecting ?? true;
    });
    io.observe(container);

    const loop = (time: number) => {
      raf = requestAnimationFrame(loop);
      if (!visible) return; // pause offscreen renders
      program.uniforms.uTime.value = time * 0.001 * speed;
      renderer.render({ scene: mesh });
    };
    raf = requestAnimationFrame(loop);

    container.appendChild(gl.canvas);

    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener('resize', resize);
      io.disconnect();
      if (gl.canvas.parentElement === container) container.removeChild(gl.canvas);
      // free WebGL resources
      const ext = gl.getExtension('WEBGL_lose_context');
      ext?.loseContext();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return <div ref={containerRef} className={`pointer-events-none ${className}`} aria-hidden />;
}
