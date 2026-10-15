import { describe, expect, it } from 'vitest';
import { DUAL_SIGN_REQUIRED, dualSignState, parseSigners, signerWording } from './approvalSigners';

const oneSigner = JSON.stringify([{ user_id: 1, role: 'admin', at: '2026-01-02T03:04:05Z' }]);

describe('parseSigners', () => {
  it('正常数组: known=true 且解析出签署人', () => {
    const r = parseSigners(oneSigner);
    expect(r.known).toBe(true);
    expect(r.signers).toHaveLength(1);
    expect(r.signers[0].user_id).toBe(1);
  });

  it('空数组: 可解析,known=true 但无签署人', () => {
    const r = parseSigners('[]');
    expect(r.known).toBe(true);
    expect(r.signers).toHaveLength(0);
  });

  it('空串: known=false 且不抛错', () => {
    expect(parseSigners('')).toEqual({ signers: [], known: false });
  });

  it('缺失: known=false 且不抛错', () => {
    expect(parseSigners(undefined)).toEqual({ signers: [], known: false });
  });

  it('不可解析: known=false 且不抛错', () => {
    expect(parseSigners('{not json')).toEqual({ signers: [], known: false });
  });

  it('可解析但非数组: known=false', () => {
    expect(parseSigners('{"a":1}')).toEqual({ signers: [], known: false });
  });
});

describe('dualSignState', () => {
  it('非 pending → decided', () => {
    expect(dualSignState({ status: 'executed', signers: oneSigner })).toBe('decided');
  });

  it('pending 且不可解析 → unknown', () => {
    expect(dualSignState({ status: 'pending', signers: '{bad' })).toBe('unknown');
  });

  it('pending 且缺失 → unknown', () => {
    expect(dualSignState({ status: 'pending' })).toBe('unknown');
  });

  it('pending 且空数组 → none', () => {
    expect(dualSignState({ status: 'pending', signers: '[]' })).toBe('none');
  });

  it('pending 且有签署 → partial', () => {
    expect(dualSignState({ status: 'pending', signers: oneSigner })).toBe('partial');
  });
});

describe('signerWording', () => {
  it('none 文案含「需 2 位批准人」', () => {
    expect(signerWording('none', 0).zh).toContain('需 2 位批准人');
  });

  it('partial 文案为 N 人已签 / 需 2 人', () => {
    expect(signerWording('partial', 1).zh).toBe('1 人已签 / 需 2 人');
  });

  it('unknown 文案为「签署状态未知」', () => {
    expect(signerWording('unknown', 0).zh).toBe('签署状态未知');
  });

  it('DUAL_SIGN_REQUIRED 为 2', () => {
    expect(DUAL_SIGN_REQUIRED).toBe(2);
  });
});
